package memfs

import (
	"testing"

	"ella.to/simplefs"
)

// BenchmarkSequentialWrite writes a 256 MiB file in 1 MiB chunks, the access
// pattern of `cp` through a mount.
func BenchmarkSequentialWrite(b *testing.B) {
	const total, chunk = 256 << 20, 1 << 20
	buf := make([]byte, chunk)
	b.SetBytes(total)
	for b.Loop() {
		m := New()
		h, err := simplefs.Create(b.Context(), m, "/big", 0)
		if err != nil {
			b.Fatal(err)
		}
		for off := int64(0); off < total; off += chunk {
			if _, err := h.WriteAt(buf, off); err != nil {
				b.Fatal(err)
			}
		}
		h.Close()
	}
}
