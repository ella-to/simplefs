package nfs

// The NFS version 3 program (RFC 1813, program 100003): every procedure the
// Linux, macOS, and Windows kernel clients use, translated onto
// simplefs.FileProvider calls.

import (
	"errors"
	"io"
	"strings"

	"ella.to/simplefs"
)

// NFSv3 procedure numbers.
const (
	nfsProcNull        = 0
	nfsProcGetattr     = 1
	nfsProcSetattr     = 2
	nfsProcLookup      = 3
	nfsProcAccess      = 4
	nfsProcReadlink    = 5
	nfsProcRead        = 6
	nfsProcWrite       = 7
	nfsProcCreate      = 8
	nfsProcMkdir       = 9
	nfsProcSymlink     = 10
	nfsProcMknod       = 11
	nfsProcRemove      = 12
	nfsProcRmdir       = 13
	nfsProcRename      = 14
	nfsProcLink        = 15
	nfsProcReaddir     = 16
	nfsProcReaddirplus = 17
	nfsProcFsstat      = 18
	nfsProcFsinfo      = 19
	nfsProcPathconf    = 20
	nfsProcCommit      = 21
)

// maxIO is the largest READ/WRITE transfer advertised and accepted.
const maxIO = 1 << 20

// fhPath decodes a filehandle argument and resolves it to a provider path.
func (s *Server) fhPath(r *xdrReader) (path string, id uint64, stat uint32) {
	fh := r.opaque()
	if r.err != nil {
		return "", 0, nfsErrBadHandle
	}
	id, ok := decodeFH(fh)
	if !ok {
		return "", 0, nfsErrBadHandle
	}
	path, ok = s.fh.path(id)
	if !ok {
		return "", 0, nfsErrStale
	}
	return path, id, nfsOK
}

// checkName validates one path component from the wire.
func checkName(name string) uint32 {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return nfsErrInval
	}
	if len(name) > 255 {
		return nfsErrNameTooL
	}
	return nfsOK
}

func (s *Server) nfsProc(c *rpcCall) []byte {
	w := replyHeader(c.xid, acceptSuccess)
	switch c.proc {
	case nfsProcNull:
	case nfsProcGetattr:
		s.opGetattr(c.args, w)
	case nfsProcSetattr:
		s.opSetattr(c.args, w)
	case nfsProcLookup:
		s.opLookup(c.args, w)
	case nfsProcAccess:
		s.opAccess(c.args, w)
	case nfsProcReadlink:
		s.opReadlink(c.args, w)
	case nfsProcRead:
		s.opRead(c.args, w)
	case nfsProcWrite:
		s.opWrite(c.args, w)
	case nfsProcCreate:
		s.opCreate(c.args, w)
	case nfsProcMkdir:
		s.opMkdir(c.args, w)
	case nfsProcSymlink:
		s.opSymlink(c.args, w)
	case nfsProcMknod:
		w.uint32(nfsErrNotSupp)
		wccNone(w)
	case nfsProcRemove:
		s.opRemove(c.args, w, false)
	case nfsProcRmdir:
		s.opRemove(c.args, w, true)
	case nfsProcRename:
		s.opRename(c.args, w)
	case nfsProcLink:
		s.opLink(c.args, w)
	case nfsProcReaddir:
		s.opReaddir(c.args, w, false)
	case nfsProcReaddirplus:
		s.opReaddir(c.args, w, true)
	case nfsProcFsstat:
		s.opFsstat(c.args, w)
	case nfsProcFsinfo:
		s.opFsinfo(c.args, w)
	case nfsProcPathconf:
		s.opPathconf(c.args, w)
	case nfsProcCommit:
		s.opCommit(c.args, w)
	default:
		return replyHeader(c.xid, acceptProcUnavail).bytes()
	}
	return w.bytes()
}

func (s *Server) opGetattr(r *xdrReader, w *xdrWriter) {
	path, id, stat := s.fhPath(r)
	if stat != nfsOK {
		w.uint32(stat)
		return
	}
	info, err := s.p.Stat(s.ctx, path)
	if err != nil {
		w.uint32(s.errStatus(err))
		return
	}
	w.uint32(nfsOK)
	s.writeFattr3(w, info, id)
}

func (s *Server) opSetattr(r *xdrReader, w *xdrWriter) {
	path, id, stat := s.fhPath(r)
	attr := readSattr3(r)
	if guard := r.bool(); guard {
		r.uint32() // guard ctime seconds
		r.uint32() // guard ctime nseconds (guard is not enforced)
	}
	if stat != nfsOK {
		w.uint32(stat)
		wccNone(w)
		return
	}
	if err := s.applySattr(path, id, attr); err != nil {
		w.uint32(s.errStatus(err))
		s.wccData(w, path, id)
		return
	}
	w.uint32(nfsOK)
	s.wccData(w, path, id)
}

func (s *Server) opLookup(r *xdrReader, w *xdrWriter) {
	dir, dirID, stat := s.fhPath(r)
	name := r.string()
	if stat != nfsOK {
		w.uint32(stat)
		postOpNone(w)
		return
	}
	var path string
	switch name {
	case ".":
		path = dir
	case "..":
		path = simplefs.ParentPath(dir)
	default:
		if st := checkName(name); st != nfsOK {
			w.uint32(st)
			s.postOpAttr(w, dir, dirID)
			return
		}
		path = simplefs.JoinPath(dir, name)
	}
	info, err := s.p.Stat(s.ctx, path)
	if err != nil {
		w.uint32(s.errStatus(err))
		s.postOpAttr(w, dir, dirID)
		return
	}
	id := s.fh.id(path)
	w.uint32(nfsOK)
	w.opaque(encodeFH(id))
	s.postOpAttrInfo(w, info, id)
	s.postOpAttr(w, dir, dirID)
}

// ACCESS3 bits.
const (
	accessRead    = 0x01
	accessLookup  = 0x02
	accessModify  = 0x04
	accessExtend  = 0x08
	accessDelete  = 0x10
	accessExecute = 0x20
)

func (s *Server) opAccess(r *xdrReader, w *xdrWriter) {
	path, id, stat := s.fhPath(r)
	req := r.uint32()
	if stat != nfsOK {
		w.uint32(stat)
		postOpNone(w)
		return
	}
	info, err := s.p.Stat(s.ctx, path)
	if err != nil {
		w.uint32(s.errStatus(err))
		postOpNone(w)
		return
	}
	w.uint32(nfsOK)
	s.postOpAttrInfo(w, info, id)
	w.uint32(req & s.accessMask(info))
}

// accessMask derives the ACCESS3 rights from the owner permission bits (the
// server reports a single owner for every file, see Options.UID) and the
// read-only state. Individual operations can still fail with ErrPermission.
func (s *Server) accessMask(info simplefs.FileInfo) uint32 {
	mode := effectiveMode(info)
	var m uint32
	if mode&0o400 != 0 {
		m |= accessRead
	}
	if mode&0o200 != 0 {
		m |= accessModify | accessExtend | accessDelete
	}
	if mode&0o100 != 0 {
		if info.IsDir() {
			m |= accessLookup
		} else {
			m |= accessExecute
		}
	}
	if info.Type.IsSymlink() {
		m |= accessRead | accessLookup
	}
	if s.readOnly() {
		m &^= accessModify | accessExtend | accessDelete
	}
	return m
}

func (s *Server) opReadlink(r *xdrReader, w *xdrWriter) {
	path, id, stat := s.fhPath(r)
	if stat != nfsOK {
		w.uint32(stat)
		postOpNone(w)
		return
	}
	sp, ok := s.p.(simplefs.SymlinkProvider)
	if !ok {
		w.uint32(nfsErrNotSupp)
		s.postOpAttr(w, path, id)
		return
	}
	target, err := sp.Readlink(s.ctx, path)
	if err != nil {
		w.uint32(s.errStatus(err))
		s.postOpAttr(w, path, id)
		return
	}
	w.uint32(nfsOK)
	s.postOpAttr(w, path, id)
	w.string(target)
}

func (s *Server) opRead(r *xdrReader, w *xdrWriter) {
	path, id, stat := s.fhPath(r)
	offset := r.uint64()
	count := r.uint32()
	if stat != nfsOK {
		w.uint32(stat)
		postOpNone(w)
		return
	}
	if count > maxIO {
		count = maxIO
	}
	h, release, err := s.files.acquire(s.ctx, id, path, false)
	if err != nil {
		w.uint32(s.errStatus(err))
		postOpNone(w)
		return
	}
	defer release()
	buf := make([]byte, count)
	n, rerr := h.ReadAt(buf, int64(offset))
	if rerr != nil && rerr != io.EOF {
		w.uint32(s.errStatus(rerr))
		s.postOpAttr(w, path, id)
		return
	}
	info, serr := h.Stat(s.ctx)
	eof := rerr == io.EOF || (serr == nil && int64(offset)+int64(n) >= info.Size)
	w.uint32(nfsOK)
	if serr == nil {
		s.postOpAttrInfo(w, info, id)
	} else {
		postOpNone(w)
	}
	w.uint32(uint32(n))
	w.bool(eof)
	w.opaque(buf[:n])
}

// WRITE stability levels.
const (
	writeUnstable = 0
	writeFileSync = 2
)

func (s *Server) opWrite(r *xdrReader, w *xdrWriter) {
	path, id, stat := s.fhPath(r)
	offset := r.uint64()
	count := r.uint32()
	stable := r.uint32()
	data := r.opaque()
	if r.err != nil {
		w.uint32(nfsErrInval)
		wccNone(w)
		return
	}
	if stat != nfsOK {
		w.uint32(stat)
		wccNone(w)
		return
	}
	if int(count) < len(data) {
		data = data[:count]
	}
	h, release, err := s.files.acquire(s.ctx, id, path, true)
	if err != nil {
		w.uint32(s.errStatus(err))
		wccNone(w)
		return
	}
	defer release()
	n, werr := h.WriteAt(data, int64(offset))
	if werr != nil {
		w.uint32(s.errStatus(werr))
		s.wccData(w, path, id)
		return
	}
	committed := uint32(writeUnstable)
	if stable != writeUnstable {
		if err := h.Sync(s.ctx); err != nil {
			w.uint32(s.errStatus(err))
			s.wccData(w, path, id)
			return
		}
		committed = writeFileSync
	}
	w.uint32(nfsOK)
	s.wccData(w, path, id)
	w.uint32(uint32(n))
	w.uint32(committed)
	w.raw(s.writeVerf[:])
}

// CREATE modes.
const (
	createUnchecked = 0
	createGuarded   = 1
	createExclusive = 2
)

func (s *Server) opCreate(r *xdrReader, w *xdrWriter) {
	dir, dirID, stat := s.fhPath(r)
	name := r.string()
	how := r.uint32()
	var attr sattr3
	var verf [8]byte
	switch how {
	case createUnchecked, createGuarded:
		attr = readSattr3(r)
	case createExclusive:
		copy(verf[:], r.bytesN(8))
	}
	if stat == nfsOK {
		stat = checkName(name)
	}
	if stat != nfsOK {
		w.uint32(stat)
		wccNone(w)
		return
	}
	path := simplefs.JoinPath(dir, name)

	mode := simplefs.FileMode(0o644)
	if attr.setMode {
		mode = simplefs.FileMode(attr.mode)
	}
	// One Open does the existence check and the creation atomically:
	// GUARDED and EXCLUSIVE fail on an existing file, UNCHECKED truncates
	// only when the client asked for size 0.
	h, err := s.p.Open(s.ctx, path, simplefs.OpenOptions{
		Read:      true,
		Write:     true,
		Create:    true,
		Exclusive: how != createUnchecked,
		Truncate:  how == createUnchecked && attr.setSize && attr.size == 0,
		Mode:      mode,
	})
	if err != nil {
		if how == createExclusive && errors.Is(err, simplefs.ErrExist) && s.sameCreateVerf(path, verf) {
			// Retransmitted exclusive create: report success again.
			s.createOK(w, path, dir, dirID)
			return
		}
		w.uint32(s.errStatus(err))
		s.wccData(w, dir, dirID)
		return
	}
	h.Close()
	id := s.fh.id(path)
	if how == createExclusive {
		s.rememberCreateVerf(path, verf)
	} else if attr.setSize && attr.size != 0 {
		if err := s.applySattr(path, id, sattr3{setSize: true, size: attr.size}); err != nil {
			w.uint32(s.errStatus(err))
			s.wccData(w, dir, dirID)
			return
		}
	}
	s.createOK(w, path, dir, dirID)
}

// createOK writes the success arm shared by CREATE and its retransmissions.
func (s *Server) createOK(w *xdrWriter, path, dir string, dirID uint64) {
	id := s.fh.id(path)
	w.uint32(nfsOK)
	postOpFH(w, id)
	s.postOpAttr(w, path, id)
	s.wccData(w, dir, dirID)
}

func (s *Server) opMkdir(r *xdrReader, w *xdrWriter) {
	dir, dirID, stat := s.fhPath(r)
	name := r.string()
	attr := readSattr3(r)
	if stat == nfsOK {
		stat = checkName(name)
	}
	if stat != nfsOK {
		w.uint32(stat)
		wccNone(w)
		return
	}
	path := simplefs.JoinPath(dir, name)
	mode := simplefs.FileMode(0o755)
	if attr.setMode {
		mode = simplefs.FileMode(attr.mode)
	}
	if err := s.p.Mkdir(s.ctx, path, simplefs.MkdirOptions{Mode: mode}); err != nil {
		w.uint32(s.errStatus(err))
		s.wccData(w, dir, dirID)
		return
	}
	s.createOK(w, path, dir, dirID)
}

func (s *Server) opSymlink(r *xdrReader, w *xdrWriter) {
	dir, dirID, stat := s.fhPath(r)
	name := r.string()
	readSattr3(r) // symlink attributes (ignored)
	target := r.string()
	if stat == nfsOK {
		stat = checkName(name)
	}
	if stat != nfsOK {
		w.uint32(stat)
		wccNone(w)
		return
	}
	sp, ok := s.p.(simplefs.SymlinkProvider)
	if !ok {
		w.uint32(nfsErrNotSupp)
		s.wccData(w, dir, dirID)
		return
	}
	path := simplefs.JoinPath(dir, name)
	if err := sp.Symlink(s.ctx, target, path); err != nil {
		w.uint32(s.errStatus(err))
		s.wccData(w, dir, dirID)
		return
	}
	s.createOK(w, path, dir, dirID)
}

func (s *Server) opRemove(r *xdrReader, w *xdrWriter, wantDir bool) {
	dir, dirID, stat := s.fhPath(r)
	name := r.string()
	if stat == nfsOK {
		stat = checkName(name)
	}
	if stat != nfsOK {
		w.uint32(stat)
		wccNone(w)
		return
	}
	path := simplefs.JoinPath(dir, name)
	if info, err := s.p.Stat(s.ctx, path); err == nil {
		if wantDir && !info.IsDir() {
			w.uint32(nfsErrNotDir)
			s.wccData(w, dir, dirID)
			return
		}
		if !wantDir && info.IsDir() {
			// RFC 1813: REMOVE on a directory. Linux uses RMDIR, but be strict.
			w.uint32(nfsErrIsDir)
			s.wccData(w, dir, dirID)
			return
		}
	}
	if err := s.p.Remove(s.ctx, path, simplefs.RemoveOptions{}); err != nil {
		w.uint32(s.errStatus(err))
		s.wccData(w, dir, dirID)
		return
	}
	if id, ok := s.fh.lookup(path); ok {
		s.files.drop(id)
	}
	s.fh.forget(path)
	s.forgetCreateVerf(path)
	w.uint32(nfsOK)
	s.wccData(w, dir, dirID)
}

func (s *Server) opRename(r *xdrReader, w *xdrWriter) {
	fromDir, fromDirID, stat1 := s.fhPath(r)
	fromName := r.string()
	toDir, toDirID, stat2 := s.fhPath(r)
	toName := r.string()
	stat := stat1
	if stat == nfsOK {
		stat = stat2
	}
	if stat == nfsOK {
		stat = checkName(fromName)
	}
	if stat == nfsOK {
		stat = checkName(toName)
	}
	if stat != nfsOK {
		w.uint32(stat)
		wccNone(w)
		wccNone(w)
		return
	}
	oldPath := simplefs.JoinPath(fromDir, fromName)
	newPath := simplefs.JoinPath(toDir, toName)
	if oldPath != newPath {
		if err := s.p.Rename(s.ctx, oldPath, newPath, simplefs.RenameOptions{Overwrite: true}); err != nil {
			w.uint32(s.errStatus(err))
			s.wccData(w, fromDir, fromDirID)
			s.wccData(w, toDir, toDirID)
			return
		}
		if id, ok := s.fh.lookup(newPath); ok {
			s.files.drop(id) // the file that was overwritten
		}
		s.fh.forget(newPath)
		s.fh.rekey(oldPath, newPath)
		s.renameCreateVerf(oldPath, newPath)
	}
	w.uint32(nfsOK)
	s.wccData(w, fromDir, fromDirID)
	s.wccData(w, toDir, toDirID)
}

func (s *Server) opLink(r *xdrReader, w *xdrWriter) {
	path, id, stat := s.fhPath(r)
	dir, dirID, stat2 := s.fhPath(r)
	name := r.string()
	if stat == nfsOK {
		stat = stat2
	}
	if stat == nfsOK {
		stat = checkName(name)
	}
	if stat != nfsOK {
		w.uint32(stat)
		postOpNone(w)
		wccNone(w)
		return
	}
	hp, ok := s.p.(simplefs.HardLinkProvider)
	if !ok {
		w.uint32(nfsErrNotSupp)
		s.postOpAttr(w, path, id)
		s.wccData(w, dir, dirID)
		return
	}
	newPath := simplefs.JoinPath(dir, name)
	if err := hp.Link(s.ctx, path, newPath); err != nil {
		w.uint32(s.errStatus(err))
		s.postOpAttr(w, path, id)
		s.wccData(w, dir, dirID)
		return
	}
	w.uint32(nfsOK)
	s.postOpAttr(w, path, id)
	s.wccData(w, dir, dirID)
}

// dirEntry is one materialized directory entry ready for wire encoding.
type dirEntry struct {
	name string
	path string
	id   uint64
	info *simplefs.FileInfo
}

// listDir drains the provider's pagination and prepends "." and "..".
func (s *Server) listDir(dir string, dirID uint64) ([]dirEntry, error) {
	parent := simplefs.ParentPath(dir)
	entries := []dirEntry{
		{name: ".", path: dir, id: dirID},
		{name: "..", path: parent, id: s.fh.id(parent)},
	}
	token := ""
	for {
		page, err := s.p.ReadDir(s.ctx, dir, simplefs.Page{Limit: 512, Token: token})
		if err != nil {
			return nil, err
		}
		for _, e := range page.Entries {
			childPath := simplefs.JoinPath(dir, e.Name)
			de := dirEntry{name: e.Name, path: childPath, id: s.fh.id(childPath)}
			if e.Info != nil {
				info := *e.Info
				info.Path = childPath
				de.info = &info
			}
			entries = append(entries, de)
		}
		if page.NextToken == "" {
			return entries, nil
		}
		token = page.NextToken
	}
}

func (s *Server) opReaddir(r *xdrReader, w *xdrWriter, plus bool) {
	dir, dirID, stat := s.fhPath(r)
	cookie := r.uint64()
	r.bytesN(8) // cookieverf (this server's listings are always verifier 0)
	var budget uint32
	if plus {
		r.uint32() // dircount
		budget = r.uint32()
	} else {
		budget = r.uint32()
	}
	if stat != nfsOK {
		w.uint32(stat)
		postOpNone(w)
		return
	}
	if budget > maxIO {
		budget = maxIO
	}
	entries, err := s.listDir(dir, dirID)
	if err != nil {
		w.uint32(s.errStatus(err))
		s.postOpAttr(w, dir, dirID)
		return
	}
	if cookie > uint64(len(entries)) {
		cookie = uint64(len(entries))
	}

	w.uint32(nfsOK)
	s.postOpAttr(w, dir, dirID)
	w.raw(make([]byte, 8)) // cookieverf3 = 0

	// Encode entries until the client's reply-size budget runs out. The
	// cookie is the 1-based index of the entry, so the next call resumes
	// right after the last one returned.
	used := uint32(128) // headroom for the fixed reply parts
	eof := true
	for i := int(cookie); i < len(entries); i++ {
		e := entries[i]
		cost := uint32(8 + 8 + 4 + pad4(len(e.name)) + 4)
		if plus {
			cost += 4 + 84 + 4 + 4 + 8
		}
		if used+cost > budget {
			eof = false
			break
		}
		used += cost
		w.bool(true) // another entry follows
		w.uint64(e.id)
		w.string(e.name)
		w.uint64(uint64(i + 1)) // cookie
		if plus {
			info := e.info
			if info == nil {
				if fi, err := s.p.Stat(s.ctx, e.path); err == nil {
					info = &fi
				}
			}
			if info != nil {
				s.postOpAttrInfo(w, *info, e.id)
			} else {
				postOpNone(w)
			}
			postOpFH(w, e.id)
		}
	}
	w.bool(false) // no more entries in this reply
	w.bool(eof)
}

func (s *Server) opFsstat(r *xdrReader, w *xdrWriter) {
	path, id, stat := s.fhPath(r)
	if stat != nfsOK {
		w.uint32(stat)
		postOpNone(w)
		return
	}
	// Generous defaults for providers without StatFS: a big, mostly-free disk.
	var total, free, avail uint64 = 1 << 42, 1 << 42, 1 << 42
	if fsinfo, ok := s.statFS(); ok && fsinfo.BlockSize > 0 {
		total = fsinfo.Blocks * fsinfo.BlockSize
		free = fsinfo.Free * fsinfo.BlockSize
		avail = fsinfo.Available * fsinfo.BlockSize
	}
	w.uint32(nfsOK)
	s.postOpAttr(w, path, id)
	w.uint64(total)   // tbytes
	w.uint64(free)    // fbytes
	w.uint64(avail)   // abytes
	w.uint64(1 << 20) // tfiles
	w.uint64(1 << 20) // ffiles
	w.uint64(1 << 20) // afiles
	w.uint32(0)       // invarsec
}

// FSINFO properties bits.
const (
	fsfLink        = 0x1
	fsfSymlink     = 0x2
	fsfHomogeneous = 0x8
	fsfCansettime  = 0x10
)

func (s *Server) opFsinfo(r *xdrReader, w *xdrWriter) {
	path, id, stat := s.fhPath(r)
	if stat != nfsOK {
		w.uint32(stat)
		postOpNone(w)
		return
	}
	props := uint32(fsfHomogeneous)
	if _, ok := s.p.(simplefs.HardLinkProvider); ok {
		props |= fsfLink
	}
	if _, ok := s.p.(simplefs.SymlinkProvider); ok {
		props |= fsfSymlink
	}
	if _, ok := s.p.(simplefs.ChtimesProvider); ok {
		props |= fsfCansettime
	}
	w.uint32(nfsOK)
	s.postOpAttr(w, path, id)
	w.uint32(maxIO)     // rtmax
	w.uint32(maxIO)     // rtpref
	w.uint32(4096)      // rtmult
	w.uint32(maxIO)     // wtmax
	w.uint32(maxIO)     // wtpref
	w.uint32(4096)      // wtmult
	w.uint32(64 * 1024) // dtpref
	w.uint64(1 << 62)   // maxfilesize
	w.uint32(0)         // time_delta seconds
	w.uint32(1)         // time_delta nseconds
	w.uint32(props)
}

func (s *Server) opPathconf(r *xdrReader, w *xdrWriter) {
	path, id, stat := s.fhPath(r)
	if stat != nfsOK {
		w.uint32(stat)
		postOpNone(w)
		return
	}
	nameMax := uint32(255)
	if fsinfo, ok := s.statFS(); ok && fsinfo.MaxNameLength > 0 {
		nameMax = fsinfo.MaxNameLength
	}
	w.uint32(nfsOK)
	s.postOpAttr(w, path, id)
	w.uint32(32767)   // linkmax
	w.uint32(nameMax) // name_max
	w.bool(true)      // no_trunc
	w.bool(false)     // chown_restricted
	w.bool(false)     // case_insensitive
	w.bool(true)      // case_preserving
}

func (s *Server) opCommit(r *xdrReader, w *xdrWriter) {
	path, id, stat := s.fhPath(r)
	r.uint64() // offset
	r.uint32() // count
	if stat != nfsOK {
		w.uint32(stat)
		wccNone(w)
		return
	}
	h, release, err := s.files.acquire(s.ctx, id, path, true)
	if err != nil {
		w.uint32(s.errStatus(err))
		wccNone(w)
		return
	}
	err = h.Sync(s.ctx)
	release()
	if err != nil {
		w.uint32(s.errStatus(err))
		s.wccData(w, path, id)
		return
	}
	w.uint32(nfsOK)
	s.wccData(w, path, id)
	w.raw(s.writeVerf[:])
}
