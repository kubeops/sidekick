/*
Copyright AppsCode Inc. and Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package snapshotserver

import "sync"

// keyedMutex is a set of mutexes, one per key, that exist only while held or
// waited for. The zero value is ready to use.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*refMutex
}

type refMutex struct {
	sync.Mutex
	refs int
}

// lock locks key and returns the function that unlocks it.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = make(map[string]*refMutex)
	}
	m, ok := k.locks[key]
	if !ok {
		m = &refMutex{}
		k.locks[key] = m
	}
	m.refs++
	k.mu.Unlock()

	m.Lock()
	return func() {
		m.Unlock()
		k.mu.Lock()
		m.refs--
		if m.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
