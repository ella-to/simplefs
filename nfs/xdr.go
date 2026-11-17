package nfs

// Minimal XDR (RFC 4506) encoder/decoder — just the subset ONC RPC, MOUNT,
// and NFSv3 need. Both types collect an error instead of returning one per
// call so protocol code reads linearly; check err once at the end.

import "errors"

var errXDR = errors.New("nfs: short or malformed XDR data")

// xdrReader decodes big-endian XDR values from a buffer.
type xdrReader struct {
	b   []byte
	off int
	err error
}

func newReader(b []byte) *xdrReader { return &xdrReader{b: b} }

func (r *xdrReader) bad() {
	if r.err == nil {
		r.err = errXDR
	}
}

// remaining reports how many undecoded bytes are left.
func (r *xdrReader) remaining() int { return len(r.b) - r.off }

func (r *xdrReader) uint32() uint32 {
	if r.err != nil || r.off+4 > len(r.b) {
		r.bad()
		return 0
	}
	v := uint32(r.b[r.off])<<24 | uint32(r.b[r.off+1])<<16 | uint32(r.b[r.off+2])<<8 | uint32(r.b[r.off+3])
	r.off += 4
	return v
}

func (r *xdrReader) uint64() uint64 {
	hi := r.uint32()
	lo := r.uint32()
	return uint64(hi)<<32 | uint64(lo)
}

func (r *xdrReader) bool() bool { return r.uint32() != 0 }

// bytesN reads n raw bytes plus XDR padding to a 4-byte boundary.
func (r *xdrReader) bytesN(n int) []byte {
	if n < 0 || r.err != nil || r.off+pad4(n) > len(r.b) {
		r.bad()
		return nil
	}
	v := r.b[r.off : r.off+n]
	r.off += pad4(n)
	return v
}

// opaque reads a variable-length opaque<> (length-prefixed, padded).
func (r *xdrReader) opaque() []byte {
	n := r.uint32()
	if n > uint32(r.remaining()) {
		r.bad()
		return nil
	}
	return r.bytesN(int(n))
}

func (r *xdrReader) string() string { return string(r.opaque()) }

// skipOpaque discards a variable-length opaque<> value.
func (r *xdrReader) skipOpaque() { r.opaque() }

// xdrWriter encodes big-endian XDR values into a growing buffer.
type xdrWriter struct {
	b []byte
}

func newWriter() *xdrWriter { return &xdrWriter{b: make([]byte, 0, 256)} }

func (w *xdrWriter) bytes() []byte { return w.b }

func (w *xdrWriter) uint32(v uint32) {
	w.b = append(w.b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func (w *xdrWriter) uint64(v uint64) {
	w.uint32(uint32(v >> 32))
	w.uint32(uint32(v))
}

func (w *xdrWriter) bool(v bool) {
	if v {
		w.uint32(1)
	} else {
		w.uint32(0)
	}
}

// raw appends bytes with no length prefix and no padding.
func (w *xdrWriter) raw(b []byte) { w.b = append(w.b, b...) }

// fixed appends bytes with padding to a 4-byte boundary but no length prefix.
func (w *xdrWriter) fixed(b []byte) {
	w.b = append(w.b, b...)
	for i := len(b); i%4 != 0; i++ {
		w.b = append(w.b, 0)
	}
}

// opaque appends a variable-length opaque<>: length prefix plus padded bytes.
func (w *xdrWriter) opaque(b []byte) {
	w.uint32(uint32(len(b)))
	w.fixed(b)
}

func (w *xdrWriter) string(s string) { w.opaque([]byte(s)) }

// pad4 rounds n up to the next multiple of 4.
func pad4(n int) int { return (n + 3) &^ 3 }
