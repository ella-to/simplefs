package nfs

// ONC RPC v2 (RFC 5531) message handling: TCP record marking, UDP datagrams,
// call decoding, and reply construction. The Server dispatches by program
// number to the PORTMAP, MOUNT, NFS, and NLM implementations.

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"sync"
	"time"

	"ella.to/simplefs/internal/bufpool"
)

// RPC program numbers served.
const (
	progPortmap = 100000 // v2
	progNFS     = 100003 // v3
	progMount   = 100005 // v3
	progNLM     = 100021 // v4
)

// RPC message types and status codes.
const (
	msgCall  = 0
	msgReply = 1

	replyAccepted = 0
	replyDenied   = 1

	acceptSuccess      = 0
	acceptProgUnavail  = 1
	acceptProgMismatch = 2
	acceptProcUnavail  = 3
	acceptGarbageArgs  = 4

	rejectRPCMismatch = 0

	authNone = 0
	authUnix = 1
)

// rpcCall is one decoded RPC call: header fields plus a reader positioned at
// the start of the procedure arguments.
type rpcCall struct {
	ctx  context.Context // cancelled with the connection or server
	xid  uint32
	prog uint32
	vers uint32
	proc uint32
	args *xdrReader
}

// decodeCall parses an RPC message. It returns nil (and no error) for
// non-call messages, which are silently dropped.
func decodeCall(msg []byte) (*rpcCall, error) {
	r := newReader(msg)
	c := &rpcCall{xid: r.uint32()}
	if mtype := r.uint32(); mtype != msgCall {
		return nil, nil
	}
	if rpcvers := r.uint32(); rpcvers != 2 {
		return c, fmt.Errorf("rpc version %d", rpcvers)
	}
	c.prog = r.uint32()
	c.vers = r.uint32()
	c.proc = r.uint32()
	r.uint32()     // cred flavor (AUTH_UNIX/AUTH_NONE — accepted either way)
	r.skipOpaque() // cred body
	r.uint32()     // verf flavor
	r.skipOpaque() // verf body
	if r.err != nil {
		return nil, r.err
	}
	c.args = r
	return c, nil
}

// replyHeader writes the standard accepted-reply header up to and including
// accept_stat; for acceptSuccess the procedure results follow.
func replyHeader(xid, stat uint32) *xdrWriter {
	w := newReplyWriter()
	w.uint32(xid)
	w.uint32(msgReply)
	w.uint32(replyAccepted)
	w.uint32(authNone) // verifier
	w.uint32(0)
	w.uint32(stat)
	return w
}

// replyProgMismatch reports that only version v of the program is served.
func replyProgMismatch(xid, v uint32) *xdrWriter {
	w := replyHeader(xid, acceptProgMismatch)
	w.uint32(v)
	w.uint32(v)
	return w
}

// replyRPCMismatch is the denied reply for an unsupported RPC version.
func replyRPCMismatch(xid uint32) *xdrWriter {
	w := newReplyWriter()
	w.uint32(xid)
	w.uint32(msgReply)
	w.uint32(replyDenied)
	w.uint32(rejectRPCMismatch)
	w.uint32(2)
	w.uint32(2)
	return w
}

// dispatch routes one RPC message to the program implementation and returns
// the encoded reply (nil to send nothing). The caller releases the reply
// once sent; msg is not retained.
func (s *Server) dispatch(ctx context.Context, client string, msg []byte) *xdrWriter {
	s.inflight.Add(1)
	defer s.inflight.Add(-1)
	c, err := decodeCall(msg)
	if c == nil {
		if err != nil {
			s.log.Debug("nfs: dropping malformed RPC", "err", err)
		}
		return nil
	}
	if err != nil {
		return replyRPCMismatch(c.xid)
	}
	if s.opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.opts.Timeout)
		defer cancel()
	}
	c.ctx = ctx
	if s.log.Enabled(ctx, slog.LevelDebug) {
		s.log.Debug("nfs: call", "xid", c.xid, "prog", c.prog, "vers", c.vers, "proc", c.proc)
	}
	switch c.prog {
	case progPortmap:
		if c.vers != 2 {
			return replyProgMismatch(c.xid, 2)
		}
		return s.portmapProc(c)
	case progMount:
		if c.vers != 3 {
			return replyProgMismatch(c.xid, 3)
		}
		return s.mountProc(c)
	case progNFS:
		if c.vers != 3 {
			return replyProgMismatch(c.xid, 3)
		}
		if !nonIdempotent(c.proc) {
			return s.nfsProc(c)
		}
		k := newDRCKey(client, c)
		if cached, dup := s.drc.begin(k); dup {
			if cached == nil {
				return nil
			}
			w := newReplyWriter()
			w.raw(cached)
			return w
		}
		w := s.nfsProc(c)
		s.drc.finish(k, w.bytes())
		return w
	case progNLM:
		if c.vers != 4 {
			return replyProgMismatch(c.xid, 4)
		}
		return s.nlmProc(c)
	default:
		return replyHeader(c.xid, acceptProgUnavail)
	}
}

// maxRecord bounds a single RPC record; the largest legitimate messages are
// WRITE calls of wtmax bytes plus headers.
const maxRecord = maxIO + 4096

// serveConn handles one TCP connection: reads records (reassembling
// fragments), dispatches each call on its own goroutine, and serializes the
// replies. NFS clients pipeline requests, so concurrent handling matters.
func (s *Server) serveConn(conn net.Conn) {
	ctx, cancel := context.WithCancel(s.ctx)
	var reqs sync.WaitGroup
	defer func() {
		cancel() // abandon in-flight provider calls of a vanished client
		conn.Close()
		reqs.Wait()
	}()
	// sem bounds concurrent requests per connection: when it is full the
	// reader stops, so a client cannot queue unbounded work.
	sem := make(chan struct{}, s.maxInFlight())
	client := clientIP(conn.RemoteAddr())
	var wmu sync.Mutex
	var hdr [4]byte
	for {
		// Records are read straight into pooled buffers: WRITE data is then
		// handed to the provider without another copy.
		var record []byte
		for {
			if _, err := io.ReadFull(conn, hdr[:]); err != nil {
				return
			}
			mark := binary.BigEndian.Uint32(hdr[:])
			n := int(mark & 0x7fffffff)
			if n > maxRecord || len(record)+n > maxRecord {
				return
			}
			last := mark&0x80000000 != 0
			start := len(record)
			if record == nil && last {
				record = bufpool.Get(n) // the common single-fragment case
			} else {
				record = slices.Grow(record, n)[:start+n]
			}
			if _, err := io.ReadFull(conn, record[start:]); err != nil {
				return
			}
			if last {
				break
			}
		}
		sem <- struct{}{}
		reqs.Add(1)
		go func(msg []byte) {
			defer func() { <-sem; reqs.Done() }()
			reply := s.dispatch(ctx, client, msg)
			bufpool.Put(msg)
			if reply == nil {
				return
			}
			defer reply.release()
			out := reply.framed()
			wmu.Lock()
			conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			_, err := conn.Write(out)
			wmu.Unlock()
			if err != nil {
				conn.Close() // a client that stopped reading is gone
			}
		}(record)
	}
}

// writeTimeout bounds how long a reply may take to send.
const writeTimeout = 30 * time.Second

// maxInFlight is Options.MaxInFlight, defaulted.
func (s *Server) maxInFlight() int {
	if s.opts.MaxInFlight > 0 {
		return s.opts.MaxInFlight
	}
	return 64
}

// servePacket handles a UDP socket: one datagram per call, one per reply.
// Windows' NFS client and rpcbind probes use UDP.
func (s *Server) servePacket(pc net.PacketConn) error {
	defer s.wg.Done()
	var reqs sync.WaitGroup
	defer reqs.Wait()
	buf := make([]byte, 65536)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if s.isClosed() {
				return ErrServerClosed
			}
			return err
		}
		if !s.allowed(addr) {
			continue
		}
		msg := make([]byte, n)
		copy(msg, buf[:n])
		reqs.Add(1)
		go func(msg []byte, addr net.Addr) {
			defer reqs.Done()
			if reply := s.dispatch(s.ctx, clientIP(addr), msg); reply != nil {
				pc.WriteTo(reply.bytes(), addr)
				reply.release()
			}
		}(msg, addr)
	}
}
