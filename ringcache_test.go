// ringcache_test.go
package ringcache_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chi07/ringcache"
)

func TestNew_InvalidCapacity(t *testing.T) {
	_, err := ringcache.New[int, string](0)
	if err == nil {
		t.Fatalf("expected error for capacity=0, got nil")
	}
	_, err = ringcache.New[int, string](-1)
	if err == nil {
		t.Fatalf("expected error for capacity<0, got nil")
	}
}

func TestNew_OK(t *testing.T) {
	rc, err := ringcache.New[int, string](3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rc.Capacity() != 3 {
		t.Fatalf("capacity mismatch: got %d, want %d", rc.Capacity(), 3)
	}
	if rc.Size() != 0 {
		t.Fatalf("initial size mismatch: got %d, want 0", rc.Size())
	}
}

func TestPushLoadHasBasic(t *testing.T) {
	rc, _ := ringcache.New[int, string](1)

	if ev := rc.Push(1, "a"); ev {
		t.Fatalf("unexpected eviction when pushing first item")
	}
	if v, ok := rc.Load(1); !ok || v != "a" {
		t.Fatalf("load mismatch: got (%v,%v), want (\"a\",true)", v, ok)
	}
	if !rc.Has(1) {
		t.Fatalf("expected Has(1)=true")
	}
	if rc.Size() != 1 {
		t.Fatalf("size mismatch: got %d, want 1", rc.Size())
	}
}

func TestEvictionAndCallback(t *testing.T) {
	var evictedCount int32
	var lastKey int
	var lastVal string

	cb := func(k int, v string) {
		atomic.AddInt32(&evictedCount, 1)
		lastKey, lastVal = k, v
	}
	rc, _ := ringcache.NewWithEvictCallback[int, string](2, cb)

	// fill
	rc.Push(1, "one")
	rc.Push(2, "two")

	// push 3 should evict 1
	if !rc.Push(3, "three") {
		t.Fatalf("expected eviction on pushing 3")
	}

	if atomic.LoadInt32(&evictedCount) != 1 {
		t.Fatalf("eviction count = %d, want 1", evictedCount)
	}
	if lastKey != 1 || lastVal != "one" {
		t.Fatalf("last evicted = (%d,%s), want (1,one)", lastKey, lastVal)
	}

	// Ensure content
	if rc.Has(1) {
		t.Fatalf("1 should be evicted")
	}
	if !rc.Has(2) || !rc.Has(3) {
		t.Fatalf("2 and 3 must exist")
	}
}

func TestReinsertSameKey_NoEviction(t *testing.T) {
	var evicted int32
	cb := func(_ int, _ string) { atomic.AddInt32(&evicted, 1) }

	rc, _ := ringcache.NewWithEvictCallback[int, string](2, cb)
	rc.Push(10, "x")
	rc.Push(20, "y")

	// Re-insert same key 10: should move it to head, NOT evict anyone
	if rc.Push(10, "z") {
		t.Fatalf("reinsert same key should not report eviction")
	}
	if atomic.LoadInt32(&evicted) != 0 {
		t.Fatalf("unexpected eviction on reinsert: %d", evicted)
	}

	// Both keys still present
	if !rc.Has(10) || !rc.Has(20) {
		t.Fatalf("expected both 10 and 20 to exist")
	}
	// Updated value
	if v, ok := rc.Load(10); !ok || v != "z" {
		t.Fatalf("10 value mismatch: got (%v,%v), want (\"z\",true)", v, ok)
	}
}

func TestDelete_Callback(t *testing.T) {
	var (
		evictedKey int
		evictedVal string
		calls      int32
	)
	cb := func(k int, v string) {
		evictedKey, evictedVal = k, v
		atomic.AddInt32(&calls, 1)
	}
	rc, _ := ringcache.NewWithEvictCallback[int, string](3, cb)

	rc.Push(1, "one")
	rc.Push(2, "two")
	rc.Push(3, "three")

	if !rc.Delete(2) {
		t.Fatalf("expected Delete(2)=true")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("callback calls = %d, want 1", calls)
	}
	if evictedKey != 2 || evictedVal != "two" {
		t.Fatalf("evicted pair = (%d,%s), want (2,two)", evictedKey, evictedVal)
	}
	if rc.Has(2) {
		t.Fatalf("2 should be removed")
	}
}

func TestClear_CallbackForAll(t *testing.T) {
	var count int32
	cb := func(_ int, _ string) {
		atomic.AddInt32(&count, 1)
	}
	rc, _ := ringcache.NewWithEvictCallback[int, string](4, cb)

	rc.Push(1, "one")
	rc.Push(2, "two")
	rc.Push(3, "three")

	if rc.Size() != 3 {
		t.Fatalf("size mismatch before clear: got %d, want 3", rc.Size())
	}

	rc.Clear()

	if rc.Size() != 0 {
		t.Fatalf("size mismatch after clear: got %d, want 0", rc.Size())
	}
	if atomic.LoadInt32(&count) != 3 {
		t.Fatalf("expected 3 callbacks on clear, got %d", count)
	}
}

func TestEvictCallbackCalledOutsideLock_NoDeadlock(t *testing.T) {
	// Verify callback runs outside the internal lock:
	// inside callback we call back into the cache; if the lock is held, this would deadlock.
	var rc *ringcache.RingCache[int, string]

	done := make(chan struct{})
	cb := func(k int, v string) {
		_ = rc.Size()
		_ = rc.Has(k)
		close(done)
	}

	var err error
	rc, err = ringcache.NewWithEvictCallback[int, string](1, cb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rc.Push(1, "one")
	rc.Push(2, "two") // triggers eviction

	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatalf("callback likely executed under lock (deadlock)")
	}
}

func TestConcurrentAccess_NoPanic(t *testing.T) {
	rc, _ := ringcache.New[int, string](10)

	var wg sync.WaitGroup
	start := make(chan struct{})

	// writers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			<-start
			for j := 0; j < 500; j++ {
				rc.Push(base*100000+j, "v")
			}
		}(i)
	}

	// readers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			<-start
			for j := 0; j < 500; j++ {
				_, _ = rc.Load(base*100000 + j)
				_ = rc.Size()
			}
		}(i)
	}

	close(start)
	wg.Wait()

	if rc.Size() > rc.Capacity() {
		t.Fatalf("size exceeds capacity: size=%d cap=%d", rc.Size(), rc.Capacity())
	}
}

func TestLoadNonExistentKey(t *testing.T) {
	rc, _ := ringcache.New[int, string](1)
	rc.Push(1, "a")

	if _, ok := rc.Load(2); ok {
		t.Fatalf("expected Load(2) to return false, but it returned true")
	}
}

func TestHasNonExistentKey(t *testing.T) {
	rc, _ := ringcache.New[int, string](1)
	rc.Push(1, "a")

	if rc.Has(2) {
		t.Fatalf("expected Has(2) to be false, but it was true")
	}
}

func TestDeleteNonExistentKey(t *testing.T) {
	var calls int32
	cb := func(_ int, _ string) { atomic.AddInt32(&calls, 1) }
	rc, _ := ringcache.NewWithEvictCallback[int, string](2, cb)
	rc.Push(1, "a")

	if rc.Delete(99) {
		t.Fatalf("expected Delete(99) to return false")
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("callback should not be called for non-existent key delete")
	}
	if rc.Size() != 1 {
		t.Fatalf("size should not change on failed delete, got %d", rc.Size())
	}
}

func TestClearEmptyCache(t *testing.T) {
	var calls int32
	cb := func(_ int, _ string) { atomic.AddInt32(&calls, 1) }
	rc, _ := ringcache.NewWithEvictCallback[int, string](3, cb)

	rc.Clear()

	if rc.Size() != 0 {
		t.Fatalf("size of empty cache after clear should be 0, got %d", rc.Size())
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("callback should not be called when clearing an empty cache")
	}
}

func TestClearWithNoCallback(t *testing.T) {
	rc, _ := ringcache.New[int, string](3)
	rc.Push(1, "a")
	rc.Push(2, "b")

	rc.Clear()

	if rc.Size() != 0 {
		t.Fatalf("size after clear should be 0, got %d", rc.Size())
	}
	if rc.Has(1) || rc.Has(2) {
		t.Fatalf("cache should be empty after clear")
	}
}

func TestPushEvictionWithNoCallback(t *testing.T) {
	rc, _ := ringcache.New[int, string](1)
	rc.Push(1, "a")

	if evicted := rc.Push(2, "b"); !evicted {
		t.Fatalf("expected eviction when pushing to a full cache")
	}

	if rc.Has(1) {
		t.Fatalf("key 1 should have been evicted")
	}
	if !rc.Has(2) {
		t.Fatalf("key 2 should be present")
	}
}
