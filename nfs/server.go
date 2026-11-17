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
	"net"
	"strconv"
	"sync"

	"ella.to/simplefs"
)

// Options tune a Server. The zero value is ready to use.
type Options struct {
	// UID and GID are reported as the owner of every file (the provider
	// model is single-user). Match these to the mounting user to avoid
	// permission surprises on Linux.
	UID, GID uint32

	// Portmap controls whether ListenAndServe also binds port 111 (both TCP
	// and UDP) and answers PORTMAP there. Windows' NFS client requires it;
	// Linux and macOS do not when given explicit port/mountport options.
	// Binding 111 may fail (privileges, rpcbind already running) — that is
	// logged nowhere and simply skipped, because it is optional.
	Portmap bool
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
	closed    bool
	done      chan struct{}
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
// there, and blocks until Close. With opts.Portmap it additionally tries to
// answer the portmapper on port 111 (best effort). addr is host:port;
// ":2049" and "127.0.0.1:0" both work.
func (s *Server) ListenAndServe(addr string) error {
	if err := s.Start(addr); err != nil {
		return err
	}
	<-s.done
	return net.ErrClosed
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
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		pc.Close()
		return net.ErrClosed
	}
	s.port = port
	s.listeners = append(s.listeners, ln)
	s.packets = append(s.packets, pc)
	s.mu.Unlock()

	go s.servePacket(pc)

	if s.opts.Portmap && port != 111 {
		if pln, err := net.Listen("tcp", net.JoinHostPort(host, "111")); err == nil {
			s.track(pln, nil)
			go s.acceptLoop(pln)
		}
		if ppc, err := net.ListenPacket("udp", net.JoinHostPort(host, "111")); err == nil {
			s.track(nil, ppc)
			go s.servePacket(ppc)
		}
	}

	go s.acceptLoop(ln)
	return nil
}

// Serve accepts TCP connections on ln and serves all RPC programs. It blocks
// until ln is closed.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.port == 0 {
		if ta, ok := ln.Addr().(*net.TCPAddr); ok {
			s.port = ta.Port
		}
	}
	s.listeners = append(s.listeners, ln)
	s.mu.Unlock()
	return s.acceptLoop(ln)
}

func (s *Server) track(ln net.Listener, pc net.PacketConn) {
	s.mu.Lock()
	if ln != nil {
		s.listeners = append(s.listeners, ln)
	}
	if pc != nil {
		s.packets = append(s.packets, pc)
	}
	s.mu.Unlock()
}

func (s *Server) acceptLoop(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.serveConn(conn)
	}
}

// Close stops all listeners and closes every cached provider handle.
func (s *Server) Close() error {
	s.mu.Lock()
	if !s.closed {
		close(s.done)
	}
	s.closed = true
	lns := s.listeners
	pcs := s.packets
	s.listeners = nil
	s.packets = nil
	s.mu.Unlock()
	for _, ln := range lns {
		ln.Close()
	}
	for _, pc := range pcs {
		pc.Close()
	}
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
