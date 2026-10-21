// SPDX-License-Identifier: Apache-2.0

package modelcheck

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
)

// hash128 is the first 128 bits of the SHA-256 of a state's Key. SHA-256 is in
// the standard library, is stable across runs and platforms (a seeded hash
// such as hash/maphash would make a collision, however unlikely, unrepeatable)
// and costs about a microsecond on a kilobyte key, which is small next to
// building the state the key describes. At 128 bits the chance of any
// collision among a billion states is around 2^-69.
type hash128 struct{ hi, lo uint64 }

func hashKey(key string, buf *[]byte) hash128 {
	// Reuse a per-worker buffer: converting the key to []byte would allocate a
	// kilobyte per successor.
	*buf = append((*buf)[:0], key...)
	sum := sha256.Sum256(*buf)
	return hash128{
		hi: binary.BigEndian.Uint64(sum[0:8]),
		lo: binary.BigEndian.Uint64(sum[8:16]),
	}
}

// keyIndex maps a state's key to its node index. By default it is an
// open-addressing table of int32 node indices that compares against the
// per-node hash column, so the index costs a few bytes per state and holds no
// key text. With exact keys it is a Go map keyed by the full Key string.
type keyIndex struct {
	table []int32 // node index + 1; 0 is empty
	mask  uint64  // len(table) - 1, kept so no int to uint64 conversion is needed
	count int
	exact map[string]int32
}

func newKeyIndex(exact bool) *keyIndex {
	if exact {
		return &keyIndex{exact: map[string]int32{}}
	}
	return &keyIndex{table: make([]int32, 1<<10), mask: 1<<10 - 1}
}

func (ix *keyIndex) lookup(h hash128, key string, hashes []hash128) (int32, bool) {
	if ix.exact != nil {
		i, ok := ix.exact[key]
		return i, ok
	}
	for pos := h.lo & ix.mask; ; pos = (pos + 1) & ix.mask {
		v := ix.table[pos]
		if v == 0 {
			return 0, false
		}
		if hashes[v-1] == h {
			return v - 1, true
		}
	}
}

// insert records node idx, whose hash is already in hashes[idx].
func (ix *keyIndex) insert(idx int32, h hash128, key string, hashes []hash128) {
	if ix.exact != nil {
		ix.exact[key] = idx
		return
	}
	if (ix.count+1)*2 > len(ix.table) {
		ix.grow(hashes)
	}
	ix.place(idx, h)
	ix.count++
}

func (ix *keyIndex) place(idx int32, h hash128) {
	pos := h.lo & ix.mask
	for ix.table[pos] != 0 {
		pos = (pos + 1) & ix.mask
	}
	ix.table[pos] = idx + 1
}

func (ix *keyIndex) grow(hashes []hash128) {
	ix.table = make([]int32, len(ix.table)*2)
	ix.mask = ix.mask<<1 | 1
	// The node being inserted is already in hashes but not yet in the table.
	for i := 0; i < ix.count; i++ {
		ix.place(int32(i), hashes[i])
	}
}

// workerPanic is what parallelFor re-raises on the calling goroutine when fn
// panicked or exited its goroutine on a worker. It keeps the original value
// reachable through Unwrap, and the worker's stack, which the caller's own
// stack would not show.
type workerPanic struct {
	item   int
	value  any
	goexit bool
	stack  string
}

func (p *workerPanic) Error() string {
	if p.goexit {
		return fmt.Sprintf(
			"modelcheck: while handling item %d, a Moves, Key, Clone, invariant or goal ended its "+
				"goroutine with runtime.Goexit (t.FailNow, t.Fatal and require.* do this) on an "+
				"exploration worker, which would silently drop the item: return an error from the "+
				"property instead, or set Options.Workers to 1\n%s", p.item, p.stack)
	}
	return fmt.Sprintf("modelcheck: panic on an exploration worker (item %d): %v\n%s",
		p.item, p.value, p.stack)
}

func (p *workerPanic) Unwrap() error {
	err, _ := p.value.(error)
	return err
}

// parallelFor calls fn(worker, i) for every i in [0, n) on up to workers
// goroutines. fn must be safe to call concurrently and write only to slots
// owned by i. A panic in fn, or fn ending its goroutine without returning, is
// re-raised on the calling goroutine as a *workerPanic, so a model bug fails
// the test that ran it instead of killing the process from a goroutine nothing
// can recover, and is never mistaken for an item that produced nothing.
//
// Items are taken in increasing order and no item below the lowest failure so
// far is skipped, so the failure reported is the lowest-numbered one whatever
// the scheduling.
func parallelFor(workers, n int, fn func(worker, i int)) {
	if workers <= 1 || n <= 1 {
		for i := range n {
			fn(0, i)
		}
		return
	}
	var (
		next atomic.Int64
		// lowest is the lowest failing item so far, n when none.
		lowest atomic.Int64
		wg     sync.WaitGroup
		mu     sync.Mutex
		fail   *workerPanic
	)
	lowest.Store(int64(n))
	record := func(p *workerPanic) {
		mu.Lock()
		defer mu.Unlock()
		if fail == nil || p.item < fail.item {
			fail = p
			lowest.Store(int64(p.item))
		}
	}
	for w := range min(workers, n) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := next.Add(1) - 1
				if i >= lowest.Load() {
					return
				}
				finished := false
				func() {
					defer func() {
						if r := recover(); r != nil {
							record(&workerPanic{
								item: int(i), value: r, stack: string(debug.Stack()),
							})
						} else if !finished {
							record(&workerPanic{
								item: int(i), goexit: true, stack: string(debug.Stack()),
							})
						}
					}()
					fn(w, int(i))
					finished = true
				}()
			}
		}()
	}
	wg.Wait()
	if fail != nil {
		panic(fail)
	}
}
