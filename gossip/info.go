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

import "strings"

// Value is satisfied by objects used within an InfoArray
// to support sorting.
type Value interface {
	Less(b Value) bool
}

// Float64Value is a float64 that satisfies the Value interface.
type Float64Value float64

// Less returns true if the receiver is less than the given Value.
func (a Float64Value) Less(b Value) bool {
	return a < b.(Float64Value)
}

// Info objects are the basic unit of information traded over the
// gossip network.
type Info struct {
	Key       string // Info key
	Val       Value  // Info value
	Timestamp int64  // Wall time at origination (Unix-nanos)
	TTLStamp  int64  // Wall time before info is discarded (Unix-nanos)
	Hops      uint32 // Number of hops from originator
	seq       int64  // Sequence number for incremental updates
}

// infoPrefix returns the text preceding the last period within
// the given key.
func infoPrefix(key string) string {
	if index := strings.LastIndex(key, "."); index != -1 {
		return key[:index]
	}
	return ""
}

// infoMap is a map of keys to Info object pointers.
type infoMap map[string]*Info

// InfoArray is a slice of Info object pointers.
type InfoArray []*Info

// Implement sort.Interface for InfoArray.
func (a InfoArray) Len() int           { return len(a) }
func (a InfoArray) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a InfoArray) Less(i, j int) bool { return a[i].Val.Less(a[j].Val) }
