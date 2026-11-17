// The exported symbols below are documented in bridge.h.
//
// Every metadata/mutation call returns a JSON "result envelope" string:
//
//	{"ok":true,  "item":{…}}            // a single item
//	{"ok":true,  "items":[{…},…]}       // a listing
//	{"ok":true}                          // success with no item (e.g. delete)
//	{"ok":false, "code":N, "msg":"…"}   // a fileprovider.Error (FPCode* in bridge.h)
//
// The Objective-C host turns a non-ok envelope into the matching NSError so
// the failure can surface in Finder. Each item carries a "perms" bitmask
// (fileprovider.Perm) the host maps to NSFileProviderItem capabilities, and
// a "parentID" so the host can model nested folders.

package capi

/*
#include <stdlib.h>

typedef void (*sfs_notify_fn)(void);

static void sfs_call_notify(sfs_notify_fn fn) { if (fn) fn(); }
*/
import "C"

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"unsafe"

	"ella.to/simplefs/fileprovider"
)

// resultDTO is the envelope wrapping every metadata/mutation response.
type resultDTO struct {
	OK    bool                `json:"ok"`
	Item  *fileprovider.Item  `json:"item,omitempty"`
	Items []fileprovider.Item `json:"items,omitempty"`
	Next  string              `json:"next,omitempty"`
	Code  int                 `json:"code,omitempty"`
	Msg   string              `json:"msg,omitempty"`
}

func marshal(r resultDTO) *C.char {
	b, err := json.Marshal(r)
	if err != nil {
		return C.CString(`{"ok":false,"code":7,"msg":"fileprovider: marshal error"}`)
	}
	return C.CString(string(b))
}

func okItem(it fileprovider.Item) *C.char {
	return marshal(resultDTO{OK: true, Item: &it})
}

func okNone() *C.char { return marshal(resultDTO{OK: true}) }

func fail(err error) *C.char {
	code, msg := fileprovider.CodeOf(err)
	return marshal(resultDTO{OK: false, Code: int(code), Msg: msg})
}

func noProvider() *C.char {
	return marshal(resultDTO{OK: false, Code: int(fileprovider.CodeIO), Msg: "fileprovider: no provider registered"})
}

// ProviderRootJSON returns the root container item. The host forces its
// identifier to the system root constant, so only the name and perms matter.
//
//export ProviderRootJSON
func ProviderRootJSON() *C.char {
	b := fileprovider.Current()
	if b == nil {
		return noProvider()
	}
	it, err := b.Root()
	if err != nil {
		return fail(err)
	}
	return okItem(it)
}

// ProviderListJSON returns one page of the children of the container with
// the given ID (empty string for the root): at most limit items (<= 0: all)
// after ctoken ("" for the first page). The envelope's "next" is the token
// of the following page, absent on the last.
//
//export ProviderListJSON
func ProviderListJSON(cParent, ctoken *C.char, limit C.int) *C.char {
	b := fileprovider.Current()
	if b == nil {
		return noProvider()
	}
	items, next, err := b.ReadDirPage(C.GoString(cParent), C.GoString(ctoken), int(limit))
	if err != nil {
		return fail(err)
	}
	return marshal(resultDTO{OK: true, Items: items, Next: next})
}

// ProviderItemJSON returns a single item's metadata as a result envelope.
//
//export ProviderItemJSON
func ProviderItemJSON(cid *C.char) *C.char {
	b := fileprovider.Current()
	if b == nil {
		return noProvider()
	}
	it, err := b.Stat(C.GoString(cid))
	if err != nil {
		return fail(err)
	}
	return okItem(it)
}

// ProviderFetchToPath streams the content of the file with the given ID
// into the local file at cdst and returns a result envelope with the item
// as of the copy. Content never passes through C memory, so files of any
// size work.
//
//export ProviderFetchToPath
func ProviderFetchToPath(cid, cdst *C.char) *C.char {
	b := fileprovider.Current()
	if b == nil {
		return noProvider()
	}
	it, err := b.FetchTo(C.GoString(cid), C.GoString(cdst))
	if err != nil {
		return fail(err)
	}
	return okItem(it)
}

// ProviderCreate handles a user-created item and returns the result
// envelope. For a file, csrc is the local file holding its content (empty
// for none), streamed into the provider.
//
//export ProviderCreate
func ProviderCreate(cname, cparent *C.char, cisDir C.int, csrc *C.char) *C.char {
	b := fileprovider.Current()
	if b == nil {
		return noProvider()
	}
	it, err := b.Create(C.GoString(cname), C.GoString(cparent), cisDir != 0, C.GoString(csrc))
	if err != nil {
		return fail(err)
	}
	return okItem(it)
}

// ProviderUpdate handles a content edit (streamed from the local file csrc),
// rename, and/or move and returns the result envelope.
//
//export ProviderUpdate
func ProviderUpdate(cid, cnewName, cnewParent *C.char, changedContent, changedName, changedParent C.int, csrc *C.char) *C.char {
	b := fileprovider.Current()
	if b == nil {
		return noProvider()
	}
	req := fileprovider.UpdateRequest{ID: C.GoString(cid)}
	if changedName != 0 {
		req.ChangedName = true
		req.NewName = C.GoString(cnewName)
	}
	if changedParent != 0 {
		req.ChangedParent = true
		req.NewParentID = C.GoString(cnewParent)
	}
	if changedContent != 0 {
		req.ChangedContent = true
		req.ContentPath = C.GoString(csrc)
	}
	it, err := b.Update(req)
	if err != nil {
		return fail(err)
	}
	return okItem(it)
}

// ProviderDelete handles a deletion and returns the result envelope (no item).
//
//export ProviderDelete
func ProviderDelete(cid *C.char) *C.char {
	b := fileprovider.Current()
	if b == nil {
		return noProvider()
	}
	if err := b.Delete(C.GoString(cid)); err != nil {
		return fail(err)
	}
	return okNone()
}

// ProviderSetHostDir records a writable directory (the extension's sandbox
// home) for use by implementations (logs, caches).
//
//export ProviderSetHostDir
func ProviderSetHostDir(cpath *C.char) {
	fileprovider.SetHostDir(C.GoString(cpath))
}

// ProviderSetDomain records the identifier of the domain this extension
// instance is serving, so implementations can scope per-mount side effects.
//
//export ProviderSetDomain
func ProviderSetDomain(cid *C.char) {
	id := C.GoString(cid)
	fileprovider.SetDomainID(id)
	// Keep item IDs across extension relaunches (best effort: without a
	// writable host dir the bridge simply starts fresh).
	if b, dir := fileprovider.Current(), fileprovider.HostDir(); b != nil && dir != "" {
		b.Persist(filepath.Join(dir, "simplefs-ids-"+safeName(id)+".log"))
	}
}

// safeName makes a domain ID usable as a file name.
func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 || r == ':' {
			return '_'
		}
		return r
	}, s)
}

// ProviderSyncAnchor returns the current change-tracking anchor.
//
//export ProviderSyncAnchor
func ProviderSyncAnchor() C.ulonglong {
	b := fileprovider.Current()
	if b == nil {
		return 0
	}
	return C.ulonglong(b.Anchor())
}

// changesDTO is the envelope ProviderChangesSince returns.
type changesDTO struct {
	OK      bool                `json:"ok"`
	Updated []fileprovider.Item `json:"updated,omitempty"`
	Deleted []string            `json:"deleted,omitempty"`
	Anchor  uint64              `json:"anchor"`
	Code    int                 `json:"code,omitempty"`
	Msg     string              `json:"msg,omitempty"`
}

// ProviderChangesSince returns the items changed after anchor as
// {"ok":true,"updated":[…],"deleted":[ids],"anchor":N}, or a failure
// envelope; code FPCodeExpired means the system must re-enumerate.
//
//export ProviderChangesSince
func ProviderChangesSince(anchor C.ulonglong) *C.char {
	b := fileprovider.Current()
	if b == nil {
		return noProvider()
	}
	updated, deleted, next, err := b.ChangesSince(uint64(anchor))
	var out changesDTO
	if err != nil {
		code, msg := fileprovider.CodeOf(err)
		out = changesDTO{Code: int(code), Msg: msg}
	} else {
		out = changesDTO{OK: true, Updated: updated, Deleted: deleted, Anchor: next}
	}
	data, _ := json.Marshal(out)
	return C.CString(string(data))
}

// ProviderTrackChanges starts change tracking; fn is called (on a Go
// thread, debounced) whenever the system should ask for changes — the host
// signals the working-set enumerator from it. Call once per process.
//
//export ProviderTrackChanges
func ProviderTrackChanges(fn C.sfs_notify_fn) {
	b := fileprovider.Current()
	if b == nil {
		return
	}
	go b.TrackChanges(context.Background(), func() { C.sfs_call_notify(fn) })
}

// ProviderFree releases a buffer returned by this package.
//
//export ProviderFree
func ProviderFree(p unsafe.Pointer) {
	C.free(p)
}
