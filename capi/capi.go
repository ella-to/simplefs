// Package capi is the CGO bridge between the Objective-C File Provider host
// (macos/ext) and the Go mount.Bridge. It is the only place that touches C
// types: it converts C arguments into Go types, calls the registered bridge,
// and converts the results back. Users never import this package directly;
// they blank-import it from their c-archive main package so its //export
// symbols are linked in (see examples/archive).
//
// Every metadata/mutation call returns a JSON "result envelope" string:
//
//	{"ok":true,  "item":{…}}            // a single item
//	{"ok":true,  "items":[{…},…]}       // a listing
//	{"ok":true}                          // success with no item (e.g. delete)
//	{"ok":false, "code":N, "msg":"…"}   // a mount.Error (FPCode* in bridge.h)
//
// The Objective-C host turns a non-ok envelope into the matching NSError so
// the failure can surface in Finder. Each item carries a "perms" bitmask
// (mount.Perm) the host maps to NSFileProviderItem capabilities, and a
// "parentID" so the host can model nested folders.
package capi

/*
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"io"
	"unsafe"

	"ella.to/simplefs/mount"
)

// resultDTO is the envelope wrapping every metadata/mutation response.
type resultDTO struct {
	OK    bool         `json:"ok"`
	Item  *mount.Item  `json:"item,omitempty"`
	Items []mount.Item `json:"items,omitempty"`
	Code  int          `json:"code,omitempty"`
	Msg   string       `json:"msg,omitempty"`
}

func marshal(r resultDTO) *C.char {
	b, err := json.Marshal(r)
	if err != nil {
		return C.CString(`{"ok":false,"code":7,"msg":"mount: marshal error"}`)
	}
	return C.CString(string(b))
}

func okItem(it mount.Item) *C.char {
	return marshal(resultDTO{OK: true, Item: &it})
}

func okItems(items []mount.Item) *C.char {
	return marshal(resultDTO{OK: true, Items: items})
}

func okNone() *C.char { return marshal(resultDTO{OK: true}) }

func fail(err error) *C.char {
	code, msg := mount.CodeOf(err)
	return marshal(resultDTO{OK: false, Code: int(code), Msg: msg})
}

func noProvider() *C.char {
	return marshal(resultDTO{OK: false, Code: int(mount.CodeIO), Msg: "mount: no provider registered"})
}

// ProviderRootJSON returns the root container item. The host forces its
// identifier to the system root constant, so only the name and perms matter.
//
//export ProviderRootJSON
func ProviderRootJSON() *C.char {
	b := mount.Current()
	if b == nil {
		return noProvider()
	}
	it, err := b.Root()
	if err != nil {
		return fail(err)
	}
	return okItem(it)
}

// ProviderListJSON returns the children of the container with the given ID
// (empty string for the root) as a result envelope.
//
//export ProviderListJSON
func ProviderListJSON(cParent *C.char) *C.char {
	b := mount.Current()
	if b == nil {
		return noProvider()
	}
	items, err := b.ReadDir(C.GoString(cParent))
	if err != nil {
		return fail(err)
	}
	return okItems(items)
}

// ProviderItemJSON returns a single item's metadata as a result envelope.
//
//export ProviderItemJSON
func ProviderItemJSON(cid *C.char) *C.char {
	b := mount.Current()
	if b == nil {
		return noProvider()
	}
	it, err := b.Stat(C.GoString(cid))
	if err != nil {
		return fail(err)
	}
	return okItem(it)
}

// ProviderContent returns the bytes of a file. The length is written to
// outLen and the mount.Code to outCode (0 == ok). On failure the return is
// NULL and, when there is a message, *outErr is a C string the caller frees
// with ProviderFree. The buffer (on success) is freed with ProviderFree.
//
//export ProviderContent
func ProviderContent(cid *C.char, outLen *C.int, outCode *C.int, outErr **C.char) unsafe.Pointer {
	*outLen = 0
	*outErr = nil
	*outCode = C.int(mount.CodeIO)

	b := mount.Current()
	if b == nil {
		*outErr = C.CString("mount: no provider registered")
		return nil
	}
	rc, err := b.Open(C.GoString(cid))
	if err != nil {
		setErr(outCode, outErr, err)
		return nil
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		setErr(outCode, outErr, err)
		return nil
	}
	*outCode = C.int(mount.CodeOK)
	*outLen = C.int(len(data))
	return C.CBytes(data)
}

func setErr(outCode *C.int, outErr **C.char, err error) {
	code, msg := mount.CodeOf(err)
	*outCode = C.int(code)
	if msg != "" {
		*outErr = C.CString(msg)
	}
}

// ProviderCreate handles a user-created item and returns the result envelope.
//
//export ProviderCreate
func ProviderCreate(cname, cparent *C.char, cisDir C.int, data unsafe.Pointer, n C.int) *C.char {
	b := mount.Current()
	if b == nil {
		return noProvider()
	}
	var content []byte
	if cisDir == 0 && data != nil && n > 0 {
		content = C.GoBytes(data, n)
	}
	it, err := b.Create(C.GoString(cname), C.GoString(cparent), cisDir != 0, content)
	if err != nil {
		return fail(err)
	}
	return okItem(it)
}

// ProviderUpdate handles a content edit, rename, and/or move and returns the
// result envelope.
//
//export ProviderUpdate
func ProviderUpdate(cid, cnewName, cnewParent *C.char, changedContent, changedName, changedParent C.int, data unsafe.Pointer, n C.int) *C.char {
	b := mount.Current()
	if b == nil {
		return noProvider()
	}
	req := mount.UpdateRequest{ID: C.GoString(cid)}
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
		if data != nil && n > 0 {
			req.Content = C.GoBytes(data, n)
		}
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
	b := mount.Current()
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
	mount.SetHostDir(C.GoString(cpath))
}

// ProviderSetDomain records the identifier of the domain this extension
// instance is serving, so implementations can scope per-mount side effects.
//
//export ProviderSetDomain
func ProviderSetDomain(cid *C.char) {
	mount.SetDomainID(C.GoString(cid))
}

// ProviderFree releases a buffer returned by this package.
//
//export ProviderFree
func ProviderFree(p unsafe.Pointer) {
	C.free(p)
}
