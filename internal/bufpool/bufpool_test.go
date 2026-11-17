package bufpool

import "testing"

func TestGetPut(t *testing.T) {
	for _, n := range []int{0, 1, 4096, 4097, 1 << 20, 1<<20 + 4096, 2 << 20} {
		b := Get(n)
		if len(b) != n {
			t.Fatalf("Get(%d) len = %d", n, len(b))
		}
		if c := cap(b); c < n || c&(c-1) != 0 {
			t.Fatalf("Get(%d) cap = %d", n, c)
		}
		Put(b)
	}
	if b := Get(3 << 20); len(b) != 3<<20 {
		t.Fatal("large Get")
	}
	Put(make([]byte, 100)) // odd capacity: ignored, must not panic
}

func BenchmarkGetPut1MiB(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		Put(Get(1 << 20))
	}
}
