package ringcache

import (
	"errors"
	"sync"
)

// EvictCallback is invoked when an entry is evicted (removed due to capacity or Delete()).
type EvictCallback[K comparable, V any] func(key K, value V)

// RingCache is a fixed-size circular buffer (ring) cache that is thread-safe.
// It keeps up to Capacity() most-recently-pushed keys in a ring layout.
// When pushing into a full slot, the existing key at that slot is evicted.
//
// Concurrency:
//   - Writers (Push/Delete/Clear) use exclusive locking.
//   - Readers (Load/Has/Size) use shared locking.
//   - onEvict is ALWAYS invoked without holding the lock.
type RingCache[K comparable, V any] struct {
	capacity int       // immutable after construction
	next     int       // next write index in the ring
	keys     []K       // ring slots for keys
	occupied []bool    // slot occupancy flags
	items    map[K]V   // key -> value
	pos      map[K]int // key -> ring slot index
	onEvict  EvictCallback[K, V]
	mu       sync.RWMutex
}

// New creates a RingCache with the given capacity (> 0).
func New[K comparable, V any](capacity int) (*RingCache[K, V], error) {
	return NewWithEvictCallback[K, V](capacity, nil)
}

// NewWithEvictCallback creates a RingCache with a given capacity and an optional eviction callback.
// The callback will be called outside the internal lock.
func NewWithEvictCallback[K comparable, V any](capacity int, cb EvictCallback[K, V]) (*RingCache[K, V], error) {
	if capacity <= 0 {
		return nil, errors.New("ringcache: capacity must be greater than zero")
	}
	return &RingCache[K, V]{
		capacity: capacity,
		next:     0,
		keys:     make([]K, capacity),
		occupied: make([]bool, capacity),
		items:    make(map[K]V, capacity),
		pos:      make(map[K]int, capacity),
		onEvict:  cb,
	}, nil
}

// Clear removes all entries from the cache.
// If an eviction callback is set, it's called for each removed entry (outside the lock).
func (c *RingCache[K, V]) Clear() {
	var toEvict []struct {
		k K
		v V
	}

	c.mu.Lock()
	// Collect items for eviction callback (if any)
	if c.onEvict != nil && len(c.items) > 0 {
		toEvict = make([]struct {
			k K
			v V
		}, 0, len(c.items))
		for k, v := range c.items {
			toEvict = append(toEvict, struct {
				k K
				v V
			}{k: k, v: v})
		}
	}

	// Re-initialize internal state
	c.items = make(map[K]V, c.capacity)
	c.pos = make(map[K]int, c.capacity)
	c.keys = make([]K, c.capacity)
	for i := range c.occupied {
		c.occupied[i] = false
	}
	c.next = 0
	c.mu.Unlock()

	// Invoke callbacks without holding the lock
	if c.onEvict != nil {
		for _, kv := range toEvict {
			c.onEvict(kv.k, kv.v)
		}
	}
}

// Push inserts (key, value) into the ring.
// If the next slot is occupied by another key, that key is evicted.
// If the key already exists, its previous slot is freed (no eviction callback) and the key is re-inserted at the head.
// Returns true if an eviction occurred.
func (c *RingCache[K, V]) Push(key K, value V) (evicted bool) {
	var (
		evictKey   *K
		evictValue V
	)

	c.mu.Lock()

	// If key already exists, free its old slot (we "move" it).
	if oldPos, exists := c.pos[key]; exists {
		c.occupied[oldPos] = false
		// Keep items[key] alive; we overwrite it below with the new value.
	}

	// If the next slot is occupied, evict the existing key at that slot.
	if c.occupied[c.next] {
		oldKey := c.keys[c.next]
		if v, ok := c.items[oldKey]; ok {
			evictKey = &oldKey
			evictValue = v
			delete(c.items, oldKey)
			delete(c.pos, oldKey)
			evicted = true
		}
	}

	// Write the new key/value into the next slot.
	c.keys[c.next] = key
	c.occupied[c.next] = true
	c.items[key] = value
	c.pos[key] = c.next
	c.next = (c.next + 1) % c.capacity

	c.mu.Unlock()

	// Call eviction callback without holding the lock.
	if evicted && c.onEvict != nil && evictKey != nil {
		c.onEvict(*evictKey, evictValue)
	}
	return
}

// Load returns (value, true) if the key exists; otherwise (zero, false).
func (c *RingCache[K, V]) Load(key K) (V, bool) {
	c.mu.RLock()
	v, ok := c.items[key]
	c.mu.RUnlock()
	return v, ok
}

// Has reports whether the key exists in the cache.
func (c *RingCache[K, V]) Has(key K) bool {
	c.mu.RLock()
	_, ok := c.items[key]
	c.mu.RUnlock()
	return ok
}

// Delete removes the key from the cache (if present) and returns true if it existed.
// The eviction callback is invoked (outside the lock) if a key was actually removed.
func (c *RingCache[K, V]) Delete(key K) bool {
	var (
		had  bool
		val  V
		call bool
	)

	c.mu.Lock()
	if p, ok := c.pos[key]; ok {
		val = c.items[key]
		delete(c.items, key)
		delete(c.pos, key)
		c.occupied[p] = false

		// Clear key slot to zero value (not required functionally, but useful for debugging/clarity).
		var zeroK K
		c.keys[p] = zeroK

		had = true
		call = c.onEvict != nil
	}
	c.mu.Unlock()

	if had && call {
		c.onEvict(key, val)
	}
	return had
}

// Size returns the current number of items in the cache.
func (c *RingCache[K, V]) Size() int {
	c.mu.RLock()
	n := len(c.items)
	c.mu.RUnlock()
	return n
}

// Capacity returns the fixed capacity of the cache.
func (c *RingCache[K, V]) Capacity() int {
	// Immutable after construction; no lock required.
	return c.capacity
}
