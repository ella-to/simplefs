package nfs

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"ella.to/simplefs"
	"ella.to/simplefs/internal/idtable"
)

// export is one provider served by a Server, with its own filehandle table
// and caches. Filehandles name their export, so several providers can share
// one server (and one port — all Windows' mount.exe can reach).
type export struct {
	srv   *Server
	index uint32
	name  string // "/" for the root export
	p     simplefs.FileProvider

	ctx       context.Context // the server's
	opts      *Options
	log       *slog.Logger
	writeVerf [8]byte
	bootID    [8]byte

	fh    *idtable.Table[uint64]
	files *openCache
	attrs *attrCache
	dirs  *dirCache

	// fsReadOnly caches FSInfo.ReadOnly from the most recent StatFS call
	// (clients issue FSSTAT/FSINFO at mount time and periodically).
	fsReadOnly atomic.Bool

	verfMu     sync.Mutex
	createVerf map[string][8]byte // path -> exclusive-create verifier
	verfOrder  []string           // insertion order, for bounding createVerf
}

func newExport(s *Server, index uint32, name string, p simplefs.FileProvider) *export {
	e := &export{
		srv:        s,
		index:      index,
		name:       name,
		p:          p,
		ctx:        s.ctx,
		opts:       &s.opts,
		log:        s.log,
		writeVerf:  s.writeVerf,
		bootID:     s.bootID,
		fh:         newFHTable(),
		files:      newOpenCache(p, s.opts.HandleIdle),
		attrs:      newAttrCache(s.opts.AttrCacheTTL),
		dirs:       newDirCache(),
		createVerf: map[string][8]byte{},
	}
	switch {
	case s.opts.MaxHandles == 0:
		e.fh.SetLimit(1<<20, e.files.inUse)
	case s.opts.MaxHandles > 0:
		e.fh.SetLimit(s.opts.MaxHandles, e.files.inUse)
	}
	if s.opts.AttrCacheTTL > 0 {
		go e.watchInvalidate()
	}
	return e
}

// fsid identifies the export in every fattr3; clients use it to tell
// filesystems apart.
func (e *export) fsid() uint64 { return fsidBase + uint64(e.index) }

// Export adds p as an additional export named name, which clients mount as
// "/name" (Linux, macOS: server:/name; Windows: \\server\name). The provider
// passed to NewServer remains the root export "/".
func (s *Server) Export(name string, p simplefs.FileProvider) error {
	name = strings.Trim(name, "/")
	if err := simplefs.ValidName(name); err != nil {
		return fmt.Errorf("nfs: export name: %w", err)
	}
	if p == nil {
		return errors.New("nfs: nil provider")
	}
	s.expMu.Lock()
	defer s.expMu.Unlock()
	for _, e := range s.exports {
		if e != nil && e.name == name {
			return fmt.Errorf("nfs: export %q: %w", name, simplefs.ErrExist)
		}
	}
	s.exports = append(s.exports, newExport(s, uint32(len(s.exports)), name, p))
	return nil
}

// Unexport removes the export named name. Its filehandles become stale and
// its cached provider handles are synced and closed.
func (s *Server) Unexport(name string) error {
	name = strings.Trim(name, "/")
	s.expMu.Lock()
	var gone *export
	for i, e := range s.exports {
		if i > 0 && e != nil && e.name == name {
			gone, s.exports[i] = e, nil
			break
		}
	}
	s.expMu.Unlock()
	if gone == nil {
		return fmt.Errorf("nfs: export %q: %w", name, simplefs.ErrNotExist)
	}
	gone.files.shutdown()
	return nil
}

// Exports returns the export names, the root export "/" first.
func (s *Server) Exports() []string {
	s.expMu.RLock()
	defer s.expMu.RUnlock()
	var out []string
	for _, e := range s.exports {
		if e != nil {
			out = append(out, e.name)
		}
	}
	return out
}

func (s *Server) root() *export {
	s.expMu.RLock()
	defer s.expMu.RUnlock()
	return s.exports[0]
}

// exportByName finds an export for a MOUNT dirpath: "/" (or "") is the root
// export, "/name" or "name" another one.
func (s *Server) exportByName(dirpath string) *export {
	name := strings.Trim(dirpath, "/")
	if name == "" {
		return s.root()
	}
	s.expMu.RLock()
	defer s.expMu.RUnlock()
	for i, e := range s.exports {
		if i > 0 && e != nil && e.name == name {
			return e
		}
	}
	return nil
}

// exportOf finds the export a wire filehandle belongs to.
func (s *Server) exportOf(fh []byte) (*export, uint32) {
	if len(fh) != fhLen {
		return nil, nfsErrBadHandle
	}
	if [8]byte(fh[8:16]) != s.bootID {
		return nil, nfsErrStale
	}
	idx := binary.BigEndian.Uint32(fh[16:])
	s.expMu.RLock()
	defer s.expMu.RUnlock()
	if int(idx) >= len(s.exports) || s.exports[idx] == nil {
		return nil, nfsErrStale
	}
	return s.exports[idx], nfsOK
}

// statFS queries the provider's StatFSProvider capability, if any, and
// refreshes the cached read-only state.
func (s *export) statFS(ctx context.Context) (simplefs.FSInfo, bool) {
	sp, ok := simplefs.As[simplefs.StatFSProvider](s.p)
	if !ok {
		return simplefs.FSInfo{}, false
	}
	info, err := sp.StatFS(ctx)
	if err != nil {
		return simplefs.FSInfo{}, false
	}
	s.fsReadOnly.Store(info.ReadOnly)
	return info, true
}

// readOnly reports whether the export must be treated as read-only.
func (s *export) readOnly() bool { return s.opts.ReadOnly || s.fsReadOnly.Load() }

// maxCreateVerfs bounds how many exclusive-create verifiers are remembered;
// a retransmission only ever arrives shortly after the original call.
const maxCreateVerfs = 1024

// sameCreateVerf reports whether path was created by an exclusive CREATE with
// this verifier (i.e. the call is a retransmission).
func (s *export) sameCreateVerf(path string, verf [8]byte) bool {
	s.verfMu.Lock()
	defer s.verfMu.Unlock()
	v, ok := s.createVerf[path]
	return ok && v == verf
}

func (s *export) rememberCreateVerf(path string, verf [8]byte) {
	s.verfMu.Lock()
	defer s.verfMu.Unlock()
	if _, ok := s.createVerf[path]; !ok {
		s.verfOrder = append(s.verfOrder, path)
	}
	s.createVerf[path] = verf
	for len(s.createVerf) > maxCreateVerfs && len(s.verfOrder) > 0 {
		oldest := s.verfOrder[0]
		s.verfOrder = s.verfOrder[1:]
		delete(s.createVerf, oldest)
	}
	if len(s.verfOrder) > 2*maxCreateVerfs {
		s.compactVerfOrderLocked()
	}
}

func (s *export) forgetCreateVerf(path string) {
	s.verfMu.Lock()
	delete(s.createVerf, path)
	s.verfMu.Unlock()
}

// renameCreateVerf moves a remembered verifier along with its file.
func (s *export) renameCreateVerf(oldPath, newPath string) {
	s.verfMu.Lock()
	defer s.verfMu.Unlock()
	delete(s.createVerf, newPath)
	if v, ok := s.createVerf[oldPath]; ok {
		delete(s.createVerf, oldPath)
		s.createVerf[newPath] = v
		s.verfOrder = append(s.verfOrder, newPath)
	}
}

// compactVerfOrderLocked drops order entries whose verifier is gone.
func (s *export) compactVerfOrderLocked() {
	seen := make(map[string]bool, len(s.createVerf))
	kept := s.verfOrder[:0]
	for _, p := range s.verfOrder {
		if _, ok := s.createVerf[p]; ok && !seen[p] {
			seen[p] = true
			kept = append(kept, p)
		}
	}
	s.verfOrder = kept
}
