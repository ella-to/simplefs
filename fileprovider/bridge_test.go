package mount

import (
	"context"
	"errors"
	"io"
	"testing"

	"ella.to/simplefs"
	"ella.to/simplefs/memfs"
)

// The bridge is exercised end-to-end against memfs through the ID-based
// calls — the same ones the macOS File Provider extension makes via capi.

func createFile(t *testing.T, b *Bridge, parentID, name, content string) Item {
	t.Helper()
	it, err := b.Create(name, parentID, false, []byte(content))
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return it
}

func readContent(t *testing.T, b *Bridge, id string) string {
	t.Helper()
	rc, err := b.Open(id)
	if err != nil {
		t.Fatalf("open %s: %v", id, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return string(data)
}

func TestBridgeRoot(t *testing.T) {
	b := NewBridge(memfs.New())
	root, err := b.Root()
	if err != nil {
		t.Fatal(err)
	}
	if root.ID != RootID || !root.Dir || root.Perms != PermRead|PermAddItems {
		t.Fatalf("root = %+v", root)
	}
}

func TestBridgeLifecycle(t *testing.T) {
	b := NewBridge(memfs.New())

	// Create a folder and a file inside it.
	folder, err := b.Create("docs", RootID, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !folder.Dir || folder.ParentID != RootID {
		t.Fatalf("folder item = %+v", folder)
	}
	if !folder.Perms.has(PermAddItems) || folder.Perms.has(PermWrite) {
		t.Fatalf("folder perms = %b", folder.Perms)
	}
	file := createFile(t, b, folder.ID, "hello.txt", "hello world")
	if file.Size != 11 || file.ParentID != folder.ID || file.Dir {
		t.Fatalf("file item = %+v", file)
	}
	if !file.Perms.has(PermRead | PermWrite | PermRename | PermMove | PermDelete) {
		t.Fatalf("file perms = %b", file.Perms)
	}

	// Listing and reading.
	items, err := b.ReadDir(folder.ID)
	if err != nil || len(items) != 1 || items[0].ID != file.ID {
		t.Fatalf("readdir = %+v, %v", items, err)
	}
	if got := readContent(t, b, file.ID); got != "hello world" {
		t.Fatalf("content = %q", got)
	}

	// Content update bumps the version.
	updated, err := b.Update(UpdateRequest{
		ID:             file.ID,
		ChangedContent: true,
		Content:        []byte("rewritten"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version == file.Version {
		t.Fatalf("version did not change on content update: %q", updated.Version)
	}
	if got := readContent(t, b, file.ID); got != "rewritten" {
		t.Fatalf("content after update = %q", got)
	}

	// Rename + move in one update; the ID must remain stable.
	moved, err := b.Update(UpdateRequest{
		ID:            file.ID,
		ChangedName:   true,
		ChangedParent: true,
		NewName:       "renamed.txt",
		NewParentID:   RootID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if moved.ID != file.ID || moved.Name != "renamed.txt" || moved.ParentID != RootID {
		t.Fatalf("moved item = %+v", moved)
	}
	if got := readContent(t, b, file.ID); got != "rewritten" {
		t.Fatalf("content after move = %q", got)
	}

	// Stat through the stable ID still works after the move.
	st, err := b.Stat(file.ID)
	if err != nil || st.Name != "renamed.txt" {
		t.Fatalf("stat after move = %+v, %v", st, err)
	}

	// Delete a folder recursively, then the IDs are gone.
	inner := createFile(t, b, folder.ID, "inner.txt", "x")
	if err := b.Delete(folder.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{folder.ID, inner.ID} {
		if _, err := b.Stat(id); err == nil {
			t.Fatalf("stat %s after delete succeeded", id)
		}
	}
}

func (p Perm) has(q Perm) bool { return p&q == q }

func TestBridgeStableIDsAcrossFolderMove(t *testing.T) {
	b := NewBridge(memfs.New())
	folder, err := b.Create("a", RootID, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	child := createFile(t, b, folder.ID, "f.txt", "data")

	// Renaming the folder must keep the child's ID resolving.
	if _, err := b.Update(UpdateRequest{ID: folder.ID, ChangedName: true, NewName: "b"}); err != nil {
		t.Fatal(err)
	}
	st, err := b.Stat(child.ID)
	if err != nil {
		t.Fatalf("child stat after folder rename: %v", err)
	}
	if got := readContent(t, b, child.ID); got != "data" {
		t.Fatalf("child content after folder rename = %q", got)
	}
	if st.ParentID != folder.ID {
		t.Fatalf("child parent = %q, want %q", st.ParentID, folder.ID)
	}
}

func TestBridgeErrorMapping(t *testing.T) {
	b := NewBridge(memfs.New())

	_, err := b.Stat("sfs-999")
	var me *Error
	if !errors.As(err, &me) || me.Code != CodeNotFound {
		t.Fatalf("stat unknown id = %v, want CodeNotFound", err)
	}

	if _, err := b.Create("dir", RootID, true, nil); err != nil {
		t.Fatal(err)
	}
	_, err = b.Create("dir", RootID, true, nil)
	if !errors.As(err, &me) || me.Code != CodeNameConflict {
		t.Fatalf("duplicate folder = %v, want CodeNameConflict", err)
	}

	if err := b.Delete(RootID); err == nil {
		t.Fatal("deleting the root succeeded")
	}
}

func TestBridgeReadDirPaginationDrained(t *testing.T) {
	b := NewBridge(memfs.New())
	// More entries than one bridge page (512) to prove pagination is drained.
	for i := range 600 {
		createFile(t, b, RootID, fmtName(i), "x")
	}
	items, err := b.ReadDir(RootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 600 {
		t.Fatalf("readdir returned %d items, want 600", len(items))
	}
}

func fmtName(i int) string {
	return "f" + string(rune('a'+i/26/26%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i%26)) + ".txt"
}

func TestCodeOfTable(t *testing.T) {
	cases := []struct {
		err  error
		want Code
	}{
		{nil, CodeOK},
		{simplefs.ErrNotExist, CodeNotFound},
		{simplefs.ErrExist, CodeNameConflict},
		{simplefs.ErrNotEmpty, CodeNameConflict},
		{simplefs.ErrIsDir, CodeUnsupported},
		{simplefs.ErrNotDir, CodeUnsupported},
		{simplefs.ErrPermission, CodeNotPermitted},
		{simplefs.ErrReadOnly, CodeNotPermitted},
		{simplefs.ErrNoSpace, CodeQuota},
		{simplefs.ErrTooLarge, CodeQuota},
		{simplefs.ErrNameTooLong, CodeUnsupported},
		{simplefs.ErrNotSupported, CodeUnsupported},
		{simplefs.ErrInvalid, CodeIO},
		{simplefs.ErrBusy, CodeServer},
		{context.DeadlineExceeded, CodeServer},
		{&Error{Code: CodeQuota, Msg: "x"}, CodeQuota},
	}
	for _, c := range cases {
		err := c.err
		if err != nil {
			err = &simplefs.PathError{Op: "op", Path: "/p", Err: err}
		}
		if got, _ := CodeOf(err); got != c.want {
			t.Errorf("CodeOf(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}
