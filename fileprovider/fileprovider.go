// Package fileprovider serves a simplefs.FileProvider through a macOS File
// Provider extension (NSFileProviderReplicatedExtension — the mechanism
// iCloud Drive and Dropbox use), giving it a Finder sidebar entry, on-demand
// downloads, and system-managed caching.
//
// For a plain mount on every OS, including macOS, use package mount instead;
// File Provider is the opt-in, Finder-integrated alternative and needs a
// signed app bundle. The moving parts, all owned by this repository:
//
//   - Bridge / Register (pure Go): adapt the path-based FileProvider onto the
//     ID-based item model the system works with.
//   - capi (cgo): //export functions the Objective-C extension calls; linked
//     in by building the registering main package as a c-archive.
//   - host/ext, host/app (Objective-C, plists, entitlements): the reusable
//     extension that forwards every system request to capi, and the
//     container app bundle.
//   - AddDomain / RemoveDomain / Domains (domain_darwin.go): manage domains
//     (mount points) from Go by wrapping NSFileProviderManager. They must run
//     from the signed container app; elsewhere they return ErrUnsupported.
//
// See the repository Makefile for the build/sign/install pipeline and
// examples/fileprovider for the c-archive entry point and container app.
package fileprovider

import (
	"context"
	"errors"
	"sync"

	"ella.to/simplefs"
)

// RootID is the identifier of the top-level container on the wire. The host
// maps the system's root-container identifier to it.
const RootID = ""

// Perm is a bitmask of the operations the user may perform on one item. It
// mirrors the FPPerm* constants in capi/bridge.h; the host maps it onto
// NSFileProviderItemCapabilities, so a cleared bit greys the operation out in
// Finder instead of merely rejecting it.
type Perm uint16

const (
	PermRead Perm = 1 << iota
	PermWrite
	PermRename
	PermMove
	PermDelete
	PermAddItems
)

// Item is the metadata shape exchanged with the host (mirrors the JSON the
// capi bridge emits).
type Item struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Dir      bool   `json:"isDir"`
	Version  string `json:"version"`
	ParentID string `json:"parentID"`
	Perms    Perm   `json:"perms"`

	// SymlinkTarget is set for symbolic links; the host presents them as
	// links (UTTypeSymbolicLink) rather than files.
	SymlinkTarget string `json:"symlinkTarget,omitempty"`
}

// Code classifies an error so the host can map it to the NSError the system
// understands. It mirrors the FPCode* constants in capi/bridge.h. The
// quota, not-permitted, name-conflict, and not-found codes surface an alert
// in Finder; the server code is treated as transient and retried silently.
type Code int

const (
	CodeOK           Code = iota // no error
	CodeNotFound                 // no item exists for the identifier
	CodeNotPermitted             // the operation is not allowed
	CodeQuota                    // a size/count/space limit was exceeded
	CodeNameConflict             // an item with that name already exists
	CodeServer                   // a transient backend failure (system retries)
	CodeUnsupported              // the provider does not support the operation
	CodeIO                       // an unclassified failure
	// CodeExpired (changes.go) follows.
)

// Error carries a Code and a human-readable message across the extension
// boundary. The bridge produces them by classifying provider errors (see
// CodeOf); a provider may also return one directly to pick the exact code —
// e.g. &fileprovider.Error{Code: fileprovider.CodeQuota, Msg: "file exceeds
// 10 MB"} — though wrapping a simplefs sentinel (ErrNoSpace, ...) is usually
// enough.
type Error struct {
	Code Code
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// CodeOf classifies err into a (Code, message) pair. A nil error is CodeOK, a
// *Error passes through, and the simplefs sentinel errors map onto the
// codes whose Finder behavior matches them; anything else is CodeIO.
func CodeOf(err error) (Code, string) {
	var me *Error
	switch {
	case err == nil:
		return CodeOK, ""
	case errors.As(err, &me):
		return me.Code, me.Msg
	case errors.Is(err, simplefs.ErrNotExist):
		return CodeNotFound, err.Error()
	case errors.Is(err, simplefs.ErrExist), errors.Is(err, simplefs.ErrNotEmpty):
		return CodeNameConflict, err.Error()
	case errors.Is(err, simplefs.ErrPermission), errors.Is(err, simplefs.ErrReadOnly):
		return CodeNotPermitted, err.Error()
	case errors.Is(err, simplefs.ErrNoSpace), errors.Is(err, simplefs.ErrTooLarge):
		return CodeQuota, err.Error()
	case errors.Is(err, simplefs.ErrNotSupported), errors.Is(err, simplefs.ErrIsDir),
		errors.Is(err, simplefs.ErrNotDir), errors.Is(err, simplefs.ErrNameTooLong):
		return CodeUnsupported, err.Error()
	case errors.Is(err, simplefs.ErrBusy), errors.Is(err, context.DeadlineExceeded):
		return CodeServer, err.Error()
	default:
		return CodeIO, err.Error()
	}
}

// Domain is a File Provider domain (one mounted folder): its identifier and
// the name shown in Finder's sidebar.
type Domain struct {
	ID          string
	DisplayName string
}

// ErrUnsupported is returned by the domain-management functions where File
// Provider is unavailable: on other operating systems, or when built without
// cgo.
var ErrUnsupported = errors.New("fileprovider: File Provider domains need macOS and cgo")

var (
	regMu    sync.RWMutex
	current  *Bridge
	hostDir  string
	domainID string
)

// Register wraps p in a Bridge and installs it as the provider served by the
// File Provider extension. Call it once from the init function of the
// c-archive main package, alongside a blank import of
// ella.to/simplefs/fileprovider/capi so the exported C symbols are linked in
// (see examples/fileprovider/archive).
func Register(p simplefs.FileProvider) *Bridge {
	b := NewBridge(p)
	regMu.Lock()
	current = b
	regMu.Unlock()
	return b
}

// Current returns the registered bridge, or nil if Register was never called.
func Current() *Bridge {
	regMu.RLock()
	defer regMu.RUnlock()
	return current
}

// SetHostDir records a writable directory provided by the host (the
// extension's sandbox home). Called by the capi bridge.
func SetHostDir(dir string) {
	regMu.Lock()
	hostDir = dir
	regMu.Unlock()
}

// HostDir returns the directory set by SetHostDir, or "" if unset (e.g. when
// running under tests rather than inside the extension). Providers can place
// logs or caches there; it is readable from outside the sandbox.
func HostDir() string {
	regMu.RLock()
	defer regMu.RUnlock()
	return hostDir
}

// SetDomainID records the identifier of the domain this extension instance is
// serving. The system runs one extension process per domain, so this lets an
// implementation scope per-mount side effects. Called by the capi bridge.
func SetDomainID(id string) {
	regMu.Lock()
	domainID = id
	regMu.Unlock()
}

// DomainID returns the identifier set by SetDomainID, or "" if unset.
func DomainID() string {
	regMu.RLock()
	defer regMu.RUnlock()
	return domainID
}
