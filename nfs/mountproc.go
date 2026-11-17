package nfs

// The MOUNT version 3 program (RFC 1813 appendix I, program 100005): hands
// out the root filehandle of the export a dirpath names — "/" for the root
// export, "/name" for one added with Server.Export.

// mnt3ErrNoEnt is MNT3ERR_NOENT: no such export.
const mnt3ErrNoEnt = 2

// MOUNT v3 procedure numbers.
const (
	mountProcNull    = 0
	mountProcMnt     = 1
	mountProcDump    = 2
	mountProcUmnt    = 3
	mountProcUmntAll = 4
	mountProcExport  = 5
)

func (s *Server) mountProc(c *rpcCall) *xdrWriter {
	w := replyHeader(c.xid, acceptSuccess)
	switch c.proc {
	case mountProcNull:
	case mountProcMnt:
		e := s.exportByName(c.args.string())
		if e == nil {
			w.uint32(mnt3ErrNoEnt)
			break
		}
		w.uint32(0) // MNT3_OK
		w.opaque(e.encodeFH(rootFileID))
		w.uint32(2) // two acceptable auth flavors:
		w.uint32(authUnix)
		w.uint32(authNone)
	case mountProcDump:
		w.bool(false) // no mount list entries
	case mountProcUmnt, mountProcUmntAll:
		// Nothing to clean up: the server keeps no per-client mount state.
	case mountProcExport:
		for _, name := range s.Exports() {
			if name != "/" {
				name = "/" + name
			}
			w.bool(true) // an export, open to every group
			w.string(name)
			w.bool(false) // no groups
		}
		w.bool(false) // no more exports
	default:
		w.release()
		return replyHeader(c.xid, acceptProcUnavail)
	}
	return w
}
