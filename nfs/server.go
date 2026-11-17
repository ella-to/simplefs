// Package nfs serves any simplefs.FileProvider over NFSv3 — a pure-Go,
// dependency-free userspace server speaking ONC RPC, PORTMAP v2, MOUNT v3,
// NFS v3, and a minimal NLM v4.
//
// Every mainstream operating system ships a kernel NFS *client*, which makes
// this the mount mechanism that needs no FUSE, no kernel modules, and no
// drivers on the serving side:
//
//   - Linux:   mount -t nfs -o vers=3,tcp,nolock,port=P,mountport=P 127.0.0.1:/ /mnt
//     (works in a minimal container — the kernel performs the MOUNT
//     handshake itself, no nfs-utils required)
//   - macOS:   mount_nfs -o vers=3,tcp,nolocks,port=P,mountport=P server:/ /mnt
//   - Windows: mount \\server\! Z:   (Client for NFS; discovers ports via the
//     portmapper, so serve one on 111)
//   - Docker:  docker volume create -d local -o type=nfs -o o=addr=...,port=P ...
//
// A Server is stateless on the wire (NFSv3): clients can reconnect at any
// time, and filehandles from a previous server process correctly come back
// NFS3ERR_STALE.
package nfs

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"ella.to/simplefs"
	"ella.to/simplefs/internal/idtable"
)

// ErrServerClosed is returned by ListenAndServe, Serve, and ServePacket after
// the server has been closed.
var ErrServerClosed = errors.New("nfs: server closed")

// Options tune a Server. The zero value is ready to use.
type Options struct {
	// UID and GID are reported as the owner of every file (the provider
	// model is single-user). Match these to the mounting user to avoid
	// permission surprises on Linux.
	UID, GID uint32

	// Portmap controls whether Start/ListenAndServe also binds port 111
	// (both TCP and UDP) and answers PORTMAP there. Windows' NFS client
	// requires it; Linux and macOS do not when given explicit port/mountport
	// options. If port 111 cannot be bound (privileges, rpcbind already
	// running) Start fails.
	Portmap bool

	// Logger receives operational messages: listeners starting and
	// stopping, bind failures, and provider errors that reach clients as a
	// generic I/O error. Nil discards them.
	Logger *slog.Logger

	// AllowedClients lists the networks clients may connect from. Empty
	// means loopback only (127.0.0.0/8 and ::1), the safe default for a
	// server with no authentication; pass 0.0.0.0/0 and ::/0 to allow
	// everyone (e.g. inside a Docker network). Connections and datagrams
	// from other addresses are dropped.
	AllowedClients []netip.Prefix

	// ReadOnly rejects every mutating procedure with NFS3ERR_ROFS before
	// it reaches the provider, and makes ACCESS deny write rights.
	ReadOnly bool

	// AttrCacheTTL enables a server-side attribute cache: Stat results are
	// reused for this long, invalidated by every change made through the
	// server and by WatchProvider events. It mainly helps providers with
	// slow Stat (remote backends). Zero disables it.
	AttrCacheTTL time.Duration

	// Timeout bounds each RPC's provider calls; a call that runs out of time
	// reaches the client as NFS3ERR_JUKEBOX ("try again later"). Zero means
	// no limit.
	Timeout time.Duration

	// MaxInFlight bounds the RPCs processed concurrently per connection.
	// Zero means 64.
	MaxInFlight int

	// MaxHandles bounds how many filehandles (path IDs) the server
	// remembers. Beyond it the least recently used are forgotten — never
	// one with an open cached provider handle — and clients holding them
	// get NFS3ERR_STALE and look the path up again. Zero means 1<<20;
	// negative means unbounded.
	MaxHandles int

	// HandleIdle is how long an open provider handle stays cached after its
	// last READ/WRITE before it is closed. Zero means 5s.
	HandleIdle time.Duration
}

func portmapErr(err error) error {
	return errors.New("nfs: Options.Portmap: cannot bind port 111 (already used by rpcbind/portmap, " +
		"or binding low ports needs privileges): " + err.Error())
}

// Server serves one simplefs.FileProvider as one NFS export ("/").
type Server struct {
	p      simplefs.FileProvider
	ctx    context.Context // cancelled by Close: aborts in-flight provider calls
	cancel context.CancelFunc
	opts   Options
	log    *slog.Logger

	fh    *idtable.Table[uint64]
	files *openCache
	attrs *attrCache
	dirs  *dirCache
	drc   *drc

	// fsReadOnly caches FSInfo.ReadOnly from the most recent StatFS call
	// (clients issue FSSTAT/FSINFO at mount time and periodically).
	fsReadOnly atomic.Bool

	writeVerf [8]byte
	bootID    [8]byte // random per Server; part of every filehandle

	verfMu     sync.Mutex
	createVerf map[string][8]byte // path -> exclusive-create verifier
	verfOrder  []string           // insertion order, for bounding createVerf

	mu        sync.Mutex
	port      int
	listeners []net.Listener
	packets   []net.PacketConn
	conns     map[net.Conn]struct{}
	closed    bool
	done      chan struct{}

	// inflight counts RPCs being processed, for Shutdown.
	inflight atomic.Int64

	// wg counts the serving goroutines (accept loops, packet loops, and
	// connections); Close waits for it so no handler outlives the server.
	wg sync.WaitGroup
}

// NewServer wraps p. Serve traffic with ListenAndServe (or Serve/ServeUDP on
// listeners you manage yourself), and Close when done.
func NewServer(p simplefs.FileProvider, opts Options) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		p:          p,
		ctx:        ctx,
		cancel:     cancel,
		opts:       opts,
		log:        log,
		fh:         newFHTable(),
		files:      newOpenCache(p, opts.HandleIdle),
		attrs:      newAttrCache(opts.AttrCacheTTL),
		dirs:       newDirCache(),
		drc:        newDRC(),
		createVerf: map[string][8]byte{},
		conns:      map[net.Conn]struct{}{},
		done:       make(chan struct{}),
	}
	rand.Read(s.writeVerf[:])
	rand.Read(s.bootID[:])
	switch {
	case opts.MaxHandles == 0:
		s.fh.SetLimit(1<<20, s.files.inUse)
	case opts.MaxHandles > 0:
		s.fh.SetLimit(opts.MaxHandles, s.files.inUse)
	}
	if opts.AttrCacheTTL > 0 {
		go s.watchInvalidate()
	}
	return s
}

// statFS queries the provider's StatFSProvider capability, if any, and
// refreshes the cached read-only state.
func (s *Server) statFS(ctx context.Context) (simplefs.FSInfo, bool) {
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
func (s *Server) readOnly() bool { return s.opts.ReadOnly || s.fsReadOnly.Load() }

// Provider returns the wrapped simplefs.FileProvider.
func (s *Server) Provider() simplefs.FileProvider { return s.p }

// Port returns the TCP/UDP port the server accepted its first listener on
// (0 before ListenAndServe/Serve).
func (s *Server) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

// Addr returns the address of the first TCP listener, or nil.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.listeners) == 0 {
		return nil
	}
	return s.listeners[0].Addr()
}

// ListenAndServe binds addr on both TCP and UDP, serves all RPC programs
// there, and blocks until Close, returning ErrServerClosed. With
// opts.Portmap it additionally answers the portmapper on port 111. addr is
// host:port; ":2049" and "127.0.0.1:0" both work.
func (s *Server) ListenAndServe(addr string) error {
	if err := s.Start(addr); err != nil {
		return err
	}
	<-s.done
	return ErrServerClosed
}

// Start is ListenAndServe without the blocking: it binds addr (TCP and UDP,
// plus the best-effort portmapper when opts.Portmap is set), spawns the
// serving goroutines, and returns once the server is reachable. Use Port to
// learn the bound port when addr asked for :0, and Close to stop.
func (s *Server) Start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.log.Error("nfs: listen failed", "addr", addr, "err", err)
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	host, _, _ := net.SplitHostPort(addr)
	udpAddr := net.JoinHostPort(host, strconv.Itoa(port))
	pc, err := net.ListenPacket("udp", udpAddr)
	if err != nil {
		ln.Close()
		return err
	}

	s.mu.Lock()
	if s.port == 0 {
		s.port = port
	}
	s.mu.Unlock()
	if !s.track(nil, pc) {
		ln.Close()
		pc.Close()
		return ErrServerClosed
	}
	go s.servePacket(pc)

	if s.opts.Portmap && port != 111 {
		pln, err := net.Listen("tcp", net.JoinHostPort(host, "111"))
		if err != nil {
			ln.Close()
			s.Close()
			s.log.Error("nfs: portmap bind failed", "err", err)
			return portmapErr(err)
		}
		ppc, err := net.ListenPacket("udp", net.JoinHostPort(host, "111"))
		if err != nil {
			pln.Close()
			ln.Close()
			s.Close()
			s.log.Error("nfs: portmap bind failed", "err", err)
			return portmapErr(err)
		}
		if s.track(pln, ppc) {
			go s.acceptLoop(pln)
			go s.servePacket(ppc)
		}
	}

	if !s.track(ln, nil) {
		ln.Close()
		return ErrServerClosed
	}
	go s.acceptLoop(ln)
	s.log.Info("nfs: serving", "addr", ln.Addr().String(), "portmap", s.opts.Portmap)
	return nil
}

// Serve accepts TCP connections on ln and serves all RPC programs. It blocks
// until ln is closed, returning ErrServerClosed after Close.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.port == 0 {
		if ta, ok := ln.Addr().(*net.TCPAddr); ok {
			s.port = ta.Port
		}
	}
	s.mu.Unlock()
	if !s.track(ln, nil) {
		ln.Close()
		return ErrServerClosed
	}
	return s.acceptLoop(ln)
}

// ServePacket serves RPC datagrams on pc (UDP). It blocks until pc is
// closed, returning ErrServerClosed after Close.
func (s *Server) ServePacket(pc net.PacketConn) error {
	if !s.track(nil, pc) {
		pc.Close()
		return ErrServerClosed
	}
	return s.servePacket(pc)
}

// track registers listeners/packet conns for Close and counts one serving
// goroutine for each. It reports false (registering nothing) once the
// server is closed.
func (s *Server) track(ln net.Listener, pc net.PacketConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if ln != nil {
		s.listeners = append(s.listeners, ln)
		s.wg.Add(1)
	}
	if pc != nil {
		s.packets = append(s.packets, pc)
		s.wg.Add(1)
	}
	return true
}

// trackConn registers an accepted connection, or reports false when the
// server is closing.
func (s *Server) trackConn(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[c] = struct{}{}
	s.wg.Add(1)
	return true
}

func (s *Server) untrackConn(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	s.wg.Done()
}

// allowed reports whether a client at addr may talk to the server.
func (s *Server) allowed(addr net.Addr) bool {
	var ip netip.Addr
	switch a := addr.(type) {
	case *net.TCPAddr:
		ip, _ = netip.AddrFromSlice(a.IP)
	case *net.UDPAddr:
		ip, _ = netip.AddrFromSlice(a.IP)
	default:
		ap, err := netip.ParseAddrPort(addr.String())
		if err != nil {
			return false
		}
		ip = ap.Addr()
	}
	ip = ip.Unmap()
	if len(s.opts.AllowedClients) == 0 {
		return ip.IsLoopback()
	}
	for _, p := range s.opts.AllowedClients {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Server) acceptLoop(ln net.Listener) error {
	defer s.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.isClosed() {
				return ErrServerClosed
			}
			return err
		}
		if !s.allowed(conn.RemoteAddr()) {
			s.log.Warn("nfs: rejected connection", "remote", conn.RemoteAddr().String())
			conn.Close()
			continue
		}
		if !s.trackConn(conn) {
			conn.Close()
			return ErrServerClosed
		}
		go func() {
			defer s.untrackConn(conn)
			s.serveConn(conn)
		}()
	}
}

// Shutdown gracefully stops the server: it stops accepting connections and
// datagrams, waits for in-flight RPCs to finish (or ctx to end), then closes
// everything like Close, syncing cached write handles first. It returns
// ctx.Err() if ctx ended before the RPCs drained.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	lns := s.listeners
	s.listeners = nil
	s.mu.Unlock()
	for _, ln := range lns {
		ln.Close()
	}

	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	var err error
	for s.inflight.Load() > 0 && err == nil {
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-t.C:
		}
	}
	if cerr := s.Close(); err == nil {
		err = cerr
	}
	return err
}

// Close immediately stops all listeners, closes every accepted connection,
// waits for in-flight handlers to return, and closes every cached provider
// handle. It is safe to call more than once.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.wg.Wait()
		return nil
	}
	s.closed = true
	close(s.done)
	s.cancel()
	lns := s.listeners
	pcs := s.packets
	conns := s.conns
	s.listeners = nil
	s.packets = nil
	s.conns = map[net.Conn]struct{}{}
	s.mu.Unlock()
	for _, ln := range lns {
		ln.Close()
	}
	for _, pc := range pcs {
		pc.Close()
	}
	for c := range conns {
		c.Close()
	}
	s.wg.Wait()
	s.files.shutdown()
	s.log.Info("nfs: server closed")
	return nil
}

// maxCreateVerfs bounds how many exclusive-create verifiers are remembered;
// a retransmission only ever arrives shortly after the original call.
const maxCreateVerfs = 1024

// sameCreateVerf reports whether path was created by an exclusive CREATE with
// this verifier (i.e. the call is a retransmission).
func (s *Server) sameCreateVerf(path string, verf [8]byte) bool {
	s.verfMu.Lock()
	defer s.verfMu.Unlock()
	v, ok := s.createVerf[path]
	return ok && v == verf
}

func (s *Server) rememberCreateVerf(path string, verf [8]byte) {
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

func (s *Server) forgetCreateVerf(path string) {
	s.verfMu.Lock()
	delete(s.createVerf, path)
	s.verfMu.Unlock()
}

// renameCreateVerf moves a remembered verifier along with its file.
func (s *Server) renameCreateVerf(oldPath, newPath string) {
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
func (s *Server) compactVerfOrderLocked() {
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
