package idtable

import (
	"strconv"
	"testing"
)

func counter() func() uint64 {
	var n uint64 = 1
	return func() uint64 { n++; return n }
}

func TestAssignLookupPath(t *testing.T) {
	tb := New[uint64](1, counter())
	if p, ok := tb.Path(1); !ok || p != "/" {
		t.Fatalf("root = %q, %v", p, ok)
	}
	a := tb.ID("/a/b/c")
	if tb.ID("/a/b/c") != a {
		t.Fatal("ID not stable")
	}
	if p, _ := tb.Path(a); p != "/a/b/c" {
		t.Fatalf("Path = %q", p)
	}
	if _, ok := tb.Lookup("/a/b"); ok {
		t.Fatal("intermediate element got an ID without being asked")
	}
	if _, ok := tb.Lookup("/zzz"); ok {
		t.Fatal("Lookup assigned an ID")
	}
}

func TestRekeyKeepsSubtreeIDs(t *testing.T) {
	tb := New[uint64](1, counter())
	dir := tb.ID("/a")
	child := tb.ID("/a/x/y")
	victim := tb.ID("/b")
	victimChild := tb.ID("/b/old")
	tb.Rekey("/a", "/b")
	if p, _ := tb.Path(dir); p != "/b" {
		t.Fatalf("dir path = %q", p)
	}
	if p, _ := tb.Path(child); p != "/b/x/y" {
		t.Fatalf("child path = %q", p)
	}
	if _, ok := tb.Path(victim); ok {
		t.Fatal("replaced destination still mapped")
	}
	if _, ok := tb.Path(victimChild); ok {
		t.Fatal("replaced destination's child still mapped")
	}
	if _, ok := tb.Lookup("/a/x/y"); ok {
		t.Fatal("old path still resolves")
	}
	tb.Rekey("/b/x/y", "/new/parent/y") // into unmapped parents
	if p, _ := tb.Path(child); p != "/new/parent/y" {
		t.Fatalf("moved into new parents: %q", p)
	}
	tb.Rekey("/missing", "/b") // unknown source: destination is replaced
	if _, ok := tb.Path(dir); ok {
		t.Fatal("destination of a rename from an unmapped path still mapped")
	}
}

func TestForget(t *testing.T) {
	tb := New[uint64](1, counter())
	a := tb.ID("/a")
	b := tb.ID("/a/b")
	keep := tb.ID("/ab")
	tb.Forget("/a")
	if _, ok := tb.Path(a); ok {
		t.Fatal("forgotten path still mapped")
	}
	if _, ok := tb.Path(b); ok {
		t.Fatal("forgotten descendant still mapped")
	}
	if p, _ := tb.Path(keep); p != "/ab" {
		t.Fatal("sibling with a shared prefix was forgotten")
	}
	if tb.Len() != 2 {
		t.Fatalf("Len = %d, want 2", tb.Len())
	}
	tb.Forget("/")
	if _, ok := tb.Path(1); !ok {
		t.Fatal("root forgotten")
	}
}

func BenchmarkRekeyLargeTable(b *testing.B) {
	tb := New[uint64](1, counter())
	for i := range 1_000_000 {
		tb.ID("/d" + strconv.Itoa(i%1000) + "/f" + strconv.Itoa(i))
	}
	b.ResetTimer()
	for i := range b.N {
		from, to := "/d1", "/moved"
		if i%2 == 1 {
			from, to = to, from
		}
		tb.Rekey(from, to)
	}
}

func BenchmarkIDAndPath(b *testing.B) {
	tb := New[uint64](1, counter())
	for i := range 100_000 {
		tb.ID("/dir/sub/f" + strconv.Itoa(i))
	}
	b.ResetTimer()
	for i := range b.N {
		id := tb.ID("/dir/sub/f" + strconv.Itoa(i%100_000))
		tb.Path(id)
	}
}
