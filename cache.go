package main

import (
	"reflect"
	"strings"
	"sync"
	"time"
)

// A cache holds what the forge said, so that what was seen once shows at once the next time: it answers with what
// it has, and when that is older than it is worth, asks again behind the scenes and tells orbit if the answer differs.
type cache struct {
	mu      sync.Mutex
	entries map[string]*entry
	now     func() time.Time
	// spawn runs the question behind the scenes: a goroutine, except in the tests
	spawn func(func())
}

type entry struct {
	val  any
	at   time.Time // when the forge said it
	used time.Time // when it was last asked for
	busy bool      // it is being asked again
}

func newCache() *cache {
	return &cache{entries: map[string]*entry{}, now: time.Now, spawn: func(f func()) { go f() }}
}

// forever is the freshness of what cannot change any more.
const forever = time.Duration(-1)

// unusedFor is how long what nobody asked for is kept.
const unusedFor = 30 * time.Minute

// cached is the value under key. With nothing kept it asks the forge and waits; with something kept it answers with
// it, and if it is older than fresh(value) asks again behind the scenes: when that differs, changed(old, new) runs.
// A failed question behind the scenes keeps what was there.
func cached[T any](c *cache, key string, fresh func(T) time.Duration, fetch func() (T, error), changed func(old, cur T)) (T, error) {
	now := c.now()
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		e.used = now
		old := e.val.(T)
		f := fresh(old)
		if f < 0 || now.Sub(e.at) < f || e.busy {
			c.mu.Unlock()
			return old, nil
		}
		e.busy = true
		c.mu.Unlock()
		c.spawn(func() {
			cur, err := fetch()
			c.mu.Lock()
			e.busy = false
			if err != nil {
				c.mu.Unlock()
				return
			}
			prev := e.val.(T)
			diff := !reflect.DeepEqual(prev, cur)
			e.val, e.at = cur, c.now()
			c.mu.Unlock()
			if diff && changed != nil {
				changed(prev, cur)
			}
		})
		return old, nil
	}
	c.mu.Unlock()

	v, err := fetch()
	if err != nil {
		var zero T
		return zero, err
	}
	c.store(key, v)
	return v, nil
}

func (c *cache) store(key string, v any) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if !e.busy && now.Sub(e.used) > unusedFor {
			delete(c.entries, k)
		}
	}
	if e, ok := c.entries[key]; ok {
		e.val, e.at, e.used = v, now, now
		return
	}
	c.entries[key] = &entry{val: v, at: now, used: now}
}

// put keeps what a poll found, and runs changed(old, new) if it is not what was kept.
func put[T any](c *cache, key string, v T, changed func(old, cur T)) {
	c.mu.Lock()
	e, ok := c.entries[key]
	var prev T
	if ok {
		prev, _ = e.val.(T)
	}
	c.mu.Unlock()
	c.store(key, v)
	if ok && changed != nil && !reflect.DeepEqual(prev, v) {
		changed(prev, v)
	}
}

// drop forgets what is kept under the keys that start with prefix, for the next question to go to the forge.
func (c *cache) drop(prefixes ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		for _, p := range prefixes {
			if strings.HasPrefix(k, p) {
				delete(c.entries, k)
				break
			}
		}
	}
}

// every is the freshness that does not depend on the value.
func every[T any](d time.Duration) func(T) time.Duration {
	return func(T) time.Duration { return d }
}
