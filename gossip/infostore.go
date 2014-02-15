// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied.  See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.

package gossip

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// InfoStore objects manage maps of Info and maps of Info Group
// objects. They maintain a sequence number generator which they use
// to allocate new info objects.
//
// InfoStores can be queried for incremental updates occurring since a
// specified sequence number.
//
// InfoStores can be combined using deltas from peer nodes.
type InfoStore struct {
	Infos  InfoMap  // Map from key to info
	Groups GroupMap // Map from key prefix to groups of infos
	MaxSeq int64    // Maximum sequence number inserted
	SeqGen int64    // Sequence generator incremented each time info is added
}

// Parameters to tune bloom filters returned by the store.
const FilterBits = 4
const FilterMaxFP = 0.025

// Returns true if the info belongs to a group registered with
// the info store; false otherwise.
func (is *InfoStore) belongsToGroup(key string) *Group {
	if group, ok := is.Groups[InfoPrefix(key)]; ok {
		return group
	}
	return nil
}

// Returns a monotonically increasing value for nanoseconds in Unix
// time. Since equal times are ignored with updates to infos, we're
// careful to avoid incorrectly ignoring a newly created value in the
// event one is created within the same nanosecond. Really unlikely
// except for the case of unittests, but better safe than sorry.
var monoTimeMu sync.Mutex
var lastTime int64

func MonotonicUnixNano() int64 {
	monoTimeMu.Lock()
	defer monoTimeMu.Unlock()

	now := time.Now().UnixNano()
	if now == lastTime {
		now = lastTime + 1
	}
	lastTime = now
	return now
}

// Create a new InfoStore.
func NewInfoStore() *InfoStore {
	return &InfoStore{make(InfoMap), make(GroupMap), 0, 0}
}

// Returns the count of infos stored in groups and the non-group infos
// map. This is really just an approximation as we don't check whether
// infos are expired.
func (is *InfoStore) InfoCount() uint32 {
	count := uint32(len(is.Infos))
	for _, group := range is.Groups {
		count += uint32(len(group.Infos))
	}
	return count
}

// Returns a new info object using specified key, value, and
// time-to-live.
func (is *InfoStore) NewInfo(key string, val Value, ttl time.Duration) *Info {
	is.SeqGen++
	now := MonotonicUnixNano()
	node := "localhost" // TODO(spencer): fix this
	return &Info{key, val, now, now + int64(ttl), is.SeqGen, node, 0}
}

// Returns an Info object by key or nil if it doesn't exist.
func (is *InfoStore) GetInfo(key string) *Info {
	if group := is.belongsToGroup(key); group != nil {
		return group.GetInfo(key)
	}
	if info, ok := is.Infos[key]; ok {
		// Check TTL and discard if too old.
		now := time.Now().UnixNano()
		if info.TTLStamp <= now {
			delete(is.Infos, key)
			return nil
		}
		return info
	}
	return nil
}

// Returns an array of info objects from specified group, sorted by
// value; sort order is dependent on group type (MIN_GROUP: ascending,
// MAX_GROUP: descending). Returns nil if group is not registered.
func (is *InfoStore) GetGroupInfos(prefix string) InfoArray {
	if group, ok := is.Groups[prefix]; ok {
		infos := group.InfosAsArray()
		switch group.TypeOf {
		case MIN_GROUP:
			sort.Sort(infos)
		case MAX_GROUP:
			sort.Sort(sort.Reverse(infos))
		}
		return infos
	}
	return nil
}

// Registers a new group with info store. Subsequent additions of
// infos will first check whether the info prefix matches a group. On
// match, the info will be added to the group instead of to the info
// map.
//
// REQUIRES: group.prefix is not already in the info store's groups map.
func (is *InfoStore) RegisterGroup(group *Group) error {
	if _, ok := is.Groups[group.Prefix]; ok {
		return fmt.Errorf("group \"%s\" already in group map", group.Prefix)
	}
	is.Groups[group.Prefix] = group
	return nil
}

// Adds or updates an info in the infos or groups maps. If the prefix
// of the info is a key of the info store's groups map, then the info
// is added to that group (prefix is defined by prefix of string up
// until last period '.'). Otherwise, the info is added to the infos
// map.
//
// Returns true if info was added; false otherwise.
func (is *InfoStore) AddInfo(info *Info) bool {
	// If the prefix matches a group, add to group.
	if group := is.belongsToGroup(info.Key); group != nil {
		if group.AddInfo(info) {
			if info.Seq > is.MaxSeq {
				is.MaxSeq = info.Seq
			}
			return true
		}
		return false
	}
	// Only replace an existing info if new timestamp is greater.
	if existingInfo, ok := is.Infos[info.Key]; ok {
		if info.Timestamp <= existingInfo.Timestamp {
			return false // Skip update, we already have newer value
		}
		// Take the minimum of the two Hops values, as that represents
		// our closest connection to the source of the info and we should
		// get credit for that when comparing peers.
		if existingInfo.Hops < info.Hops {
			info.Hops = existingInfo.Hops
		}
	}
	// Update info map.
	is.Infos[info.Key] = info
	if info.Seq > is.MaxSeq {
		is.MaxSeq = info.Seq
	}
	return true
}

// Visitor pattern to run two methods in the course of visiting all
// groups, all group infos, and all non-group infos. The visitGroup
// function is run against each group in turn. After each group is
// visited, the visitInfo function is run against each of its infos.
// Finally, after all groups have been visitied, the visitInfo
// function is run against each non-group info in turn.
func (is *InfoStore) visitInfos(visitGroup func(*Group) error, visitInfo func(*Info) error) error {
	for _, group := range is.Groups {
		if visitGroup != nil {
			if err := visitGroup(group); err != nil {
				return err
			}
		}
		if visitInfo != nil {
			for _, info := range group.Infos {
				if err := visitInfo(info); err != nil {
					return err
				}
			}
		}
	}

	if visitInfo != nil {
		for _, info := range is.Infos {
			if err := visitInfo(info); err != nil {
				return err
			}
		}
	}

	return nil
}

// Combines an incremental delta with the current info store.
// The sequence numbers on all info objects are reset using the
// info store's sequence generator. All hop distances on infos
// are incremented to indicate they've arrived from an external
// source.
func (is *InfoStore) Combine(delta *InfoStore) error {
	// Combine group info. If the group doesn't yet exist, register
	// it. Extract the infos from the group and combine them
	// one-by-one using AddInfo.
	return delta.visitInfos(func(group *Group) error {
		if _, ok := is.Groups[group.Prefix]; !ok {
			// Make a copy of the group.
			groupCopy, err := NewGroup(group.Prefix, group.Limit, group.TypeOf)
			if err != nil {
				return err
			}
			is.RegisterGroup(groupCopy)
		}
		return nil
	}, func(info *Info) error {
		is.SeqGen++
		info.Seq = is.SeqGen
		info.Hops++
		is.AddInfo(info)
		return nil
	})
}

// Returns an incremental delta of infos added to the info store since
// (not including) the specified sequence number. These deltas are
// intended for efficiently updating peer nodes.
//
// Returns an error if there are no deltas.
func (is *InfoStore) Delta(seq int64) (*InfoStore, error) {
	if seq >= is.MaxSeq {
		return nil, fmt.Errorf("no deltas to info store since sequence number %d", seq)
	}

	delta := NewInfoStore()
	delta.SeqGen = is.SeqGen

	// Compute delta of groups and infos.
	err := is.visitInfos(func(group *Group) error {
		deltaGroup, err := NewGroup(group.Prefix, group.Limit, group.TypeOf)
		if err != nil {
			return err
		}
		delta.RegisterGroup(deltaGroup)
		return nil
	}, func(info *Info) error {
		if info.Seq > seq {
			delta.AddInfo(info)
		}
		return nil
	})

	return delta, err
}

// Builds a bloom filter containing the keys held in the store which
// arrived within the specified number of maximum hops. Filters are
// passed to peer nodes in order to evaluate gossip candidates.
func (is *InfoStore) BuildFilter(maxHops uint32) (*Filter, error) {
	f, err := NewFilter(is.InfoCount(), FilterBits, FilterMaxFP)
	if err != nil {
		return nil, err
	}

	err = is.visitInfos(nil, func(info *Info) error {
		if info.Hops <= maxHops {
			f.AddKey(info.Key)
		}
		return nil
	})

	return f, err
}

// Compares the supplied filter with the contents of this infostore.
// Each key from the infostore is "removed" from the filter, to
// yield a filter with remains approximately equal to the keys
// contained in the filter but not present in the infostore.
func (is *InfoStore) DiffFilter(f *Filter, maxHops uint32) (uint32, error) {
	err := is.visitInfos(nil, func(info *Info) error {
		if info.Hops <= maxHops {
			f.RemoveKey(info.Key)
		}
		return nil
	})
	return f.ApproximateInsertions(), err
}
