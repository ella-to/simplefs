// Package bufpool recycles byte slices in power-of-two size classes, so hot
// I/O paths (RPC records, READ data) do not allocate per request.
package bufpool

import (
	"math/bits"
	"sync"
)

const (
	minShift = 12 // 4 KiB
	maxShift = 21 // 2 MiB
)

var pools [maxShift - minShift + 1]sync.Pool

// class returns the size-class index for n bytes, or -1 if n is too large
// to pool.
func class(n int) int {
	if n <= 1<<minShift {
		return 0
	}
	shift := bits.Len(uint(n - 1))
	if shift > maxShift {
		return -1
	}
	return shift - minShift
}

// Get returns a slice of length n. Its capacity is n rounded up to a size
// class; slices larger than 2 MiB are allocated directly.
func Get(n int) []byte {
	c := class(n)
	if c < 0 {
		return make([]byte, n)
	}
	if p, ok := pools[c].Get().(*[]byte); ok {
		return (*p)[:n]
	}
	return make([]byte, n, 1<<(c+minShift))
}

// Put returns b to its pool. Slices whose capacity is not exactly a size
// class (not obtained from Get, or reallocated by append) are dropped. The
// caller must not use b afterwards.
func Put(b []byte) {
	c := cap(b)
	if c < 1<<minShift || c > 1<<maxShift || c&(c-1) != 0 {
		return
	}
	b = b[:0]
	pools[bits.Len(uint(c))-1-minShift].Put(&b)
}
