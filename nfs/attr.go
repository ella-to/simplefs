package nfs

// NFSv3 (RFC 1813) attribute encoding and error translation.

import (
	"errors"
	"time"

	"ella.to/simplefs"
)

// nfsstat3 values.
const (
	nfsOK             = 0
	nfsErrPerm        = 1
	nfsErrNoEnt       = 2
	nfsErrIO          = 5
	nfsErrAcces       = 13
	nfsErrExist       = 17
	nfsErrNotDir      = 20
	nfsErrIsDir       = 21
	nfsErrInval       = 22
	nfsErrFBig        = 27
	nfsErrNoSpc       = 28
	nfsErrROFS        = 30
	nfsErrNameTooL    = 63
	nfsErrNotEmpty    = 66
	nfsErrDQuot       = 69
	nfsErrStale       = 70
	nfsErrBadHandle   = 10001
	nfsErrNotSupp     = 10004
	nfsErrServerFault = 10006
	nfsErrBadType     = 10007
)

// errStatus translates a provider error into an nfsstat3 code, logging
// errors that have no precise NFS equivalent (they reach the client as EIO,
// so the log is the only place their cause survives).
func (s *Server) errStatus(err error) uint32 {
	st := statusOf(err)
	if st == nfsErrIO {
		s.log.Warn("nfs: provider error reported as EIO", "err", err)
	}
	return st
}

// statusOf translates a provider error into an nfsstat3 code.
func statusOf(err error) uint32 {
	switch {
	case err == nil:
		return nfsOK
	case errors.Is(err, simplefs.ErrNotExist):
		return nfsErrNoEnt
	case errors.Is(err, simplefs.ErrExist):
		return nfsErrExist
	case errors.Is(err, simplefs.ErrIsDir):
		return nfsErrIsDir
	case errors.Is(err, simplefs.ErrNotDir):
		return nfsErrNotDir
	case errors.Is(err, simplefs.ErrNotEmpty):
		return nfsErrNotEmpty
	case errors.Is(err, simplefs.ErrPermission):
		return nfsErrAcces
	case errors.Is(err, simplefs.ErrNotSupported):
		return nfsErrNotSupp
	case errors.Is(err, simplefs.ErrInvalid):
		return nfsErrInval
	case errors.Is(err, simplefs.ErrNoSpace):
		return nfsErrNoSpc
	default:
		return nfsErrIO
	}
}

// ftype3 values.
const (
	typeReg = 1
	typeDir = 2
	typeLnk = 5
)

func ftype(t simplefs.FileType) uint32 {
	switch {
	case t.IsDir():
		return typeDir
	case t.IsSymlink():
		return typeLnk
	default:
		return typeReg
	}
}

// writeTime encodes an nfstime3.
func writeTime(w *xdrWriter, t time.Time) {
	if t.IsZero() {
		w.uint32(0)
		w.uint32(0)
		return
	}
	w.uint32(uint32(t.Unix()))
	w.uint32(uint32(t.Nanosecond()))
}

// effectiveMode is the permission bits reported for info: the provider's,
// or a conventional default when the provider leaves them zero.
func effectiveMode(info simplefs.FileInfo) uint32 {
	mode := uint32(info.Mode) & 0o7777
	if mode == 0 {
		switch {
		case info.Type.IsDir():
			mode = 0o755
		case info.Type.IsSymlink():
			mode = 0o777
		default:
			mode = 0o644
		}
	}
	return mode
}

// writeFattr3 encodes the full fattr3 for info, using id as fileid3.
func (s *Server) writeFattr3(w *xdrWriter, info simplefs.FileInfo, id uint64) {
	mode := effectiveMode(info)
	nlink := uint32(1)
	if info.Type.IsDir() {
		nlink = 2
	}
	w.uint32(ftype(info.Type))
	w.uint32(mode)
	w.uint32(nlink)
	w.uint32(s.opts.UID)
	w.uint32(s.opts.GID)
	w.uint64(uint64(info.Size))            // size
	w.uint64(uint64(info.Size+511) &^ 511) // used (rounded to 512)
	w.uint32(0)                            // rdev major
	w.uint32(0)                            // rdev minor
	w.uint64(fsid)                         // fsid
	w.uint64(id)                           // fileid
	writeTime(w, info.ModTime)             // atime
	writeTime(w, info.ModTime)             // mtime
	writeTime(w, info.ModTime)             // ctime
}

// fsid identifies this filesystem in every fattr3 ("SFS1" as an integer).
const fsid = 0x53465331

// postOpAttr encodes a post_op_attr: stat the path and attach attributes if
// that works, else "attributes_follow = false".
func (s *Server) postOpAttr(w *xdrWriter, path string, id uint64) {
	info, err := s.p.Stat(s.ctx, path)
	if err != nil {
		w.bool(false)
		return
	}
	w.bool(true)
	s.writeFattr3(w, info, id)
}

// postOpAttrInfo encodes a post_op_attr from an already-fetched FileInfo.
func (s *Server) postOpAttrInfo(w *xdrWriter, info simplefs.FileInfo, id uint64) {
	w.bool(true)
	s.writeFattr3(w, info, id)
}

// postOpNone encodes an absent post_op_attr.
func postOpNone(w *xdrWriter) { w.bool(false) }

// wccData encodes wcc_data with no pre-op attributes and best-effort post-op
// attributes for path (RFC 1813 allows both sides to be absent).
func (s *Server) wccData(w *xdrWriter, path string, id uint64) {
	w.bool(false) // pre_op_attr absent
	s.postOpAttr(w, path, id)
}

// wccNone encodes wcc_data with neither side present.
func wccNone(w *xdrWriter) {
	w.bool(false)
	w.bool(false)
}

// postOpFH encodes a post_op_fh3 carrying the handle for id.
func postOpFH(w *xdrWriter, id uint64) {
	w.bool(true)
	w.opaque(encodeFH(id))
}

// sattr3 carries the settable attributes of a SETATTR/CREATE/MKDIR call.
type sattr3 struct {
	setMode bool
	mode    uint32
	setUID  bool
	uid     uint32
	setGID  bool
	gid     uint32
	setSize bool
	size    uint64
	// set_atime/set_mtime: 0 = don't change, 1 = server time, 2 = client time
	atimeHow uint32
	atime    time.Time
	mtimeHow uint32
	mtime    time.Time
}

func readSattr3(r *xdrReader) sattr3 {
	var a sattr3
	if a.setMode = r.bool(); a.setMode {
		a.mode = r.uint32()
	}
	if a.setUID = r.bool(); a.setUID {
		a.uid = r.uint32()
	}
	if a.setGID = r.bool(); a.setGID {
		a.gid = r.uint32()
	}
	if a.setSize = r.bool(); a.setSize {
		a.size = r.uint64()
	}
	a.atimeHow = r.uint32()
	if a.atimeHow == 2 {
		sec := r.uint32()
		nsec := r.uint32()
		a.atime = time.Unix(int64(sec), int64(nsec))
	}
	a.mtimeHow = r.uint32()
	if a.mtimeHow == 2 {
		sec := r.uint32()
		nsec := r.uint32()
		a.mtime = time.Unix(int64(sec), int64(nsec))
	}
	return a
}

// apply carries out the attribute changes a client requested. Ownership
// changes are quietly ignored (single-user server); mode and times are
// forwarded when the provider implements the matching capability.
func (s *Server) applySattr(path string, id uint64, a sattr3) error {
	if a.setSize {
		h, release, err := s.files.acquire(s.ctx, id, path, true)
		if err != nil {
			return err
		}
		err = h.Truncate(s.ctx, int64(a.size))
		release()
		if err != nil {
			return err
		}
	}
	if a.setMode {
		if cp, ok := s.p.(simplefs.ChmodProvider); ok {
			if err := cp.Chmod(s.ctx, path, simplefs.FileMode(a.mode)); err != nil && !errors.Is(err, simplefs.ErrNotSupported) {
				return err
			}
		}
	}
	if a.atimeHow != 0 || a.mtimeHow != 0 {
		if tp, ok := s.p.(simplefs.ChtimesProvider); ok {
			now := time.Now()
			atime, mtime := a.atime, a.mtime
			if a.atimeHow == 1 {
				atime = now
			}
			if a.mtimeHow == 1 {
				mtime = now
			}
			// A timestamp the client did not set stays zero, which
			// ChtimesProvider defines as "leave unchanged".
			if err := tp.Chtimes(s.ctx, path, atime, mtime); err != nil && !errors.Is(err, simplefs.ErrNotSupported) {
				return err
			}
		}
	}
	return nil
}
