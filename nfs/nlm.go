package nfs

// A minimal NLM version 4 program (program 100021): the byte-range locking
// side protocol some clients (notably Windows) insist on speaking. The
// provider model has no distributed locks, so every request is granted
// optimistically — the same behavior as mounting a normal NFS server with
// local-lock semantics.

// NLM v4 procedure numbers (the subset clients send to a server).
const (
	nlmProcNull   = 0
	nlmProcTest   = 1
	nlmProcLock   = 2
	nlmProcCancel = 3
	nlmProcUnlock = 4
	nlmProcFree   = 20 // FREE_ALL
)

// nlm4 status codes.
const nlmGranted = 0

func (s *Server) nlmProc(c *rpcCall) *xdrWriter {
	w := replyHeader(c.xid, acceptSuccess)
	switch c.proc {
	case nlmProcNull, nlmProcFree:
	case nlmProcTest:
		cookie := c.args.opaque()
		w.opaque(cookie)
		w.uint32(nlmGranted) // nlm4_testres: no conflicting holder
	case nlmProcLock, nlmProcCancel, nlmProcUnlock:
		cookie := c.args.opaque()
		w.opaque(cookie)
		w.uint32(nlmGranted)
	default:
		w.release()
		return replyHeader(c.xid, acceptProcUnavail)
	}
	return w
}
