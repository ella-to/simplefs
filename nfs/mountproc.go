package nfs

// The MOUNT version 3 program (RFC 1813 appendix I, program 100005): hands
// out the root filehandle. Every dirpath is accepted and maps to the
// provider's root — the server exposes exactly one export.

// MOUNT v3 procedure numbers.
const (
	mountProcNull    = 0
	mountProcMnt     = 1
	mountProcDump    = 2
	mountProcUmnt    = 3
	mountProcUmntAll = 4
	mountProcExport  = 5
)

func (s *Server) mountProc(c *rpcCall) []byte {
	w := replyHeader(c.xid, acceptSuccess)
	switch c.proc {
	case mountProcNull:
	case mountProcMnt:
		c.args.string() // dirpath — single export, always the root
		w.uint32(0)     // MNT3_OK
		w.opaque(encodeFH(rootFileID))
		w.uint32(2) // two acceptable auth flavors:
		w.uint32(authUnix)
		w.uint32(authNone)
	case mountProcDump:
		w.bool(false) // no mount list entries
	case mountProcUmnt, mountProcUmntAll:
		// Nothing to clean up: the server keeps no per-client mount state.
	case mountProcExport:
		w.bool(true) // one export: "/" open to every group
		w.string("/")
		w.bool(false) // no groups
		w.bool(false) // no more exports
	default:
		return replyHeader(c.xid, acceptProcUnavail).bytes()
	}
	return w.bytes()
}
