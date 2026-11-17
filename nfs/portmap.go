package nfs

// The PORTMAP version 2 program (RFC 1833, program 100000): tells clients
// which port each RPC program listens on. Linux and macOS can be pointed at
// explicit ports (mount -o port=...,mountport=...), but Windows' NFS client
// discovers everything through the portmapper, so the server answers for all
// the programs it serves. SET/UNSET are refused — this is not a system
// rpcbind.

const (
	pmapProcNull    = 0
	pmapProcSet     = 1
	pmapProcUnset   = 2
	pmapProcGetport = 3
	pmapProcDump    = 4

	protoTCP = 6
	protoUDP = 17
)

func (s *Server) portmapProc(c *rpcCall) *xdrWriter {
	w := replyHeader(c.xid, acceptSuccess)
	switch c.proc {
	case pmapProcNull:
	case pmapProcSet, pmapProcUnset:
		w.bool(false)
	case pmapProcGetport:
		prog := c.args.uint32()
		c.args.uint32() // version (any version maps to the same socket)
		proto := c.args.uint32()
		c.args.uint32() // suggested port
		port := uint32(0)
		if proto == protoTCP || proto == protoUDP {
			switch prog {
			case progNFS, progMount, progNLM:
				port = uint32(s.Port())
			case progPortmap:
				port = 111
			}
		}
		w.uint32(port)
	case pmapProcDump:
		port := uint32(s.Port())
		for _, prog := range []uint32{progNFS, progMount, progNLM} {
			for _, vers := range programVersions(prog) {
				for _, proto := range []uint32{protoTCP, protoUDP} {
					w.bool(true)
					w.uint32(prog)
					w.uint32(vers)
					w.uint32(proto)
					w.uint32(port)
				}
			}
		}
		w.bool(false)
	default:
		w.release()
		return replyHeader(c.xid, acceptProcUnavail)
	}
	return w
}

func programVersions(prog uint32) []uint32 {
	if prog == progNLM {
		return []uint32{4}
	}
	return []uint32{3}
}
