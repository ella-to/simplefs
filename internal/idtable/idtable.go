// Package idtable maps stable identifiers to paths and back for backends
// that address files by ID (NFS filehandles, File Provider item IDs) on top
// of the path-based simplefs.FileProvider.
//
// Entries live in a trie of path elements with parent pointers, so a rename
// re-parents one node — O(depth), however large the moved subtree — and
// IDs stay attached to the objects they name.
package idtable

import (
	"strings"
	"sync"
)

// Table maps IDs to clean, absolute paths ("/a/b") and back. It is safe for
// concurrent use.
type Table[ID comparable] struct {
	mu   sync.Mutex
	root *node[ID]
	byID map[ID]*node[ID]
	next func() ID
}

type node[ID comparable] struct {
	name     string
	parent   *node[ID]
	children map[string]*node[ID]
	id       ID
	hasID    bool
}

// New returns a table in which "/" has rootID; next generates the IDs of
// newly seen paths and must never repeat one.
func New[ID comparable](rootID ID, next func() ID) *Table[ID] {
	root := &node[ID]{id: rootID, hasID: true}
	return &Table[ID]{
		root: root,
		byID: map[ID]*node[ID]{rootID: root},
		next: next,
	}
}

// elems splits a clean absolute path into its elements.
func elems(path string) []string {
	if path == "/" || path == "" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(path, "/"), "/")
}

// find returns the node for path, creating missing nodes when create is set.
func (t *Table[ID]) find(path string, create bool) *node[ID] {
	n := t.root
	for _, e := range elems(path) {
		c, ok := n.children[e]
		if !ok {
			if !create {
				return nil
			}
			c = &node[ID]{name: e, parent: n}
			if n.children == nil {
				n.children = map[string]*node[ID]{}
			}
			n.children[e] = c
		}
		n = c
	}
	return n
}

// ID returns the ID of path, assigning a new one on first sight.
func (t *Table[ID]) ID(path string) ID {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.find(path, true)
	if !n.hasID {
		n.id, n.hasID = t.next(), true
		t.byID[n.id] = n
	}
	return n.id
}

// Lookup returns the ID already assigned to path, without assigning one.
func (t *Table[ID]) Lookup(path string) (ID, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := t.find(path, false); n != nil && n.hasID {
		return n.id, true
	}
	var zero ID
	return zero, false
}

// Path resolves id to its current path; ok is false for unknown (stale) IDs.
func (t *Table[ID]) Path(id ID) (path string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.byID[id]
	if !ok {
		return "", false
	}
	return pathOf(n), true
}

func pathOf[ID comparable](n *node[ID]) string {
	if n.parent == nil {
		return "/"
	}
	var parts []string
	for ; n.parent != nil; n = n.parent {
		parts = append(parts, n.name)
	}
	var b strings.Builder
	for i := len(parts) - 1; i >= 0; i-- {
		b.WriteByte('/')
		b.WriteString(parts[i])
	}
	return b.String()
}

// Rekey moves oldPath, with its whole subtree, to newPath; IDs are kept.
// Whatever was mapped at newPath is forgotten (it was replaced).
func (t *Table[ID]) Rekey(oldPath, newPath string) {
	if oldPath == newPath || oldPath == "/" || newPath == "/" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.find(oldPath, false)
	if dst := t.find(newPath, false); dst != nil && dst != n {
		t.forgetLocked(dst)
	}
	if n == nil {
		return
	}
	newParent := t.find(parentOf(newPath), true)
	old := n.parent
	delete(old.children, n.name)
	n.name = baseOf(newPath)
	n.parent = newParent
	if newParent.children == nil {
		newParent.children = map[string]*node[ID]{}
	}
	newParent.children[n.name] = n
	t.prune(old)
}

// Forget drops path and every mapped descendant.
func (t *Table[ID]) Forget(path string) {
	if path == "/" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := t.find(path, false); n != nil {
		t.forgetLocked(n)
	}
}

func (t *Table[ID]) forgetLocked(n *node[ID]) {
	var drop func(*node[ID])
	drop = func(n *node[ID]) {
		if n.hasID {
			delete(t.byID, n.id)
		}
		for _, c := range n.children {
			drop(c)
		}
	}
	drop(n)
	parent := n.parent
	delete(parent.children, n.name)
	t.prune(parent)
}

// prune removes ID-less leaf nodes left behind, walking up from n.
func (t *Table[ID]) prune(n *node[ID]) {
	for n != nil && n.parent != nil && !n.hasID && len(n.children) == 0 {
		delete(n.parent.children, n.name)
		n = n.parent
	}
}

// Len reports how many IDs are mapped, the root included.
func (t *Table[ID]) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.byID)
}

func parentOf(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

func baseOf(p string) string { return p[strings.LastIndexByte(p, '/')+1:] }
