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
	"net"
	"strconv"
	"sync"

	"ella.to/simplefs"
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
}

func portmapErr(err error) error {
	return errors.New("nfs: Options.Portmap: cannot bind port 111 (already used by rpcbind/portmap, " +
		"or binding low ports needs privileges): " + err.Error())
}

// Server serves one simplefs.FileProvider as one NFS export ("/").
type Server struct {
	p    simplefs.FileProvider
	ctx  context.Context
	opts Options

	fh    *fhTable
	files *openCache

	writeVerf [8]byte

	verfMu     sync.Mutex
	createVerf map[string][8]byte

	mu        sync.Mutex
	port      int
	listeners []net.Listener
	packets   []net.PacketConn
	conns     map[net.Conn]struct{}
	closed    bool
	done      chan struct{}

	// wg counts the serving goroutines (accept loops, packet loops, and
	// connections); Close waits for it so no handler outlives the server.
	wg sync.WaitGroup
}

// NewServer wraps p. Serve traffic with ListenAndServe (or Serve/ServeUDP on
// listeners you manage yourself), and Close when done.
func NewServer(p simplefs.FileProvider, opts Options) *Server {
	s := &Server{
		p:          p,
		ctx:        context.Background(),
		opts:       opts,
		fh:         newFHTable(),
		files:      newOpenCache(p),
		createVerf: map[string][8]byte{},
		conns:      map[net.Conn]struct{}{},
		done:       make(chan struct{}),
	}
	rand.Read(s.writeVerf[:])
	return s
}

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
			return portmapErr(err)
		}
		ppc, err := net.ListenPacket("udp", net.JoinHostPort(host, "111"))
		if err != nil {
			pln.Close()
			ln.Close()
			s.Close()
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
	return nil
}

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
	s.createVerf[path] = verf
	s.verfMu.Unlock()
}

func (s *Server) forgetCreateVerf(path string) {
	s.verfMu.Lock()
	delete(s.createVerf, path)
	s.verfMu.Unlock()
}
