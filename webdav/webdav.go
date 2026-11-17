// Package webdav serves a simplefs.FileProvider over WebDAV (RFC 4918) using
// only the standard library.
//
// Every desktop OS ships a WebDAV client — Windows' WebClient service
// (`net use`, Explorer), macOS Finder (`mount_webdav`), Linux davfs2/GVfs —
// so WebDAV is the fallback where NFS is unavailable, notably Windows Home.
//
// The handler implements class 1 (OPTIONS, PROPFIND, PROPPATCH, GET/HEAD
// with ranges, PUT, DELETE, MKCOL, COPY, MOVE) and advertises class 2 with
// stub locks: LOCK always succeeds with a fresh token, UNLOCK always
// succeeds. Windows and macOS refuse to write to servers without locking,
// and simplefs providers have no locks to enforce. Like the nfs package it
// has no authentication and only accepts loopback clients by default.
package webdav

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"ella.to/simplefs"
)

// Options configure a Handler. The zero value serves read-write to loopback
// clients only.
type Options struct {
	// ReadOnly rejects every mutation with 403 Forbidden.
	ReadOnly bool

	// AllowedClients lists the networks requests may come from. Empty means
	// loopback only; the server has no authentication.
	AllowedClients []netip.Prefix

	// Logger receives request failures. Nil discards them.
	Logger *slog.Logger
}

// Handler serves a provider over WebDAV. Create it with NewHandler.
type Handler struct {
	p    simplefs.FileProvider
	opts Options
	log  *slog.Logger
}

var _ http.Handler = (*Handler)(nil)

// NewHandler returns an http.Handler serving p at the URL root.
func NewHandler(p simplefs.FileProvider, opts Options) *Handler {
	if opts.ReadOnly {
		p = simplefs.ReadOnly(p)
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Handler{p: p, opts: opts, log: log}
}

// ServeHTTP dispatches one WebDAV request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.allowed(r.RemoteAddr) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	p := simplefs.CleanPath(r.URL.Path)
	var status int
	var err error
	switch r.Method {
	case http.MethodOptions:
		status = h.options(w)
	case http.MethodGet, http.MethodHead:
		status, err = h.get(w, r, p)
	case http.MethodPut:
		status, err = h.put(w, r, p)
	case http.MethodDelete:
		status, err = h.delete(r, p)
	case "MKCOL":
		status, err = h.mkcol(r, p)
	case "COPY", "MOVE":
		status, err = h.copyMove(r, p)
	case "PROPFIND":
		status, err = h.propfind(w, r, p)
	case "PROPPATCH":
		status, err = h.proppatch(w, r, p)
	case "LOCK":
		status, err = h.lock(w, r, p)
	case "UNLOCK":
		status = http.StatusNoContent
	default:
		status = http.StatusMethodNotAllowed
	}
	if err != nil {
		status = statusOf(err)
		if status >= 500 {
			h.log.Warn("webdav: request failed", "method", r.Method, "path", p, "err", err)
		}
	}
	if status != 0 {
		w.WriteHeader(status)
	}
}

func (h *Handler) allowed(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	if len(h.opts.AllowedClients) == 0 {
		return ip.IsLoopback()
	}
	for _, p := range h.opts.AllowedClients {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// statusOf maps provider errors onto HTTP status codes.
func statusOf(err error) int {
	switch {
	case errors.Is(err, simplefs.ErrNotExist):
		return http.StatusNotFound
	case errors.Is(err, simplefs.ErrExist):
		return http.StatusPreconditionFailed
	case errors.Is(err, simplefs.ErrNotEmpty), errors.Is(err, simplefs.ErrIsDir), errors.Is(err, simplefs.ErrNotDir):
		return http.StatusConflict
	case errors.Is(err, simplefs.ErrPermission), errors.Is(err, simplefs.ErrReadOnly):
		return http.StatusForbidden
	case errors.Is(err, simplefs.ErrNoSpace):
		return http.StatusInsufficientStorage
	case errors.Is(err, simplefs.ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, simplefs.ErrNameTooLong):
		return http.StatusRequestURITooLong
	case errors.Is(err, simplefs.ErrNotSupported):
		return http.StatusNotImplemented
	case errors.Is(err, simplefs.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, simplefs.ErrBusy), errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func (h *Handler) options(w http.ResponseWriter) int {
	w.Header().Set("DAV", "1, 2")
	w.Header().Set("MS-Author-Via", "DAV")
	w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, MKCOL, COPY, MOVE, PROPFIND, PROPPATCH, LOCK, UNLOCK")
	return http.StatusOK
}

// etag returns the entity tag for info.
func etag(info simplefs.FileInfo) string {
	if info.ETag != "" {
		return strconv.Quote(info.ETag)
	}
	return fmt.Sprintf(`"%x-%x"`, info.ModTime.UnixNano(), info.Size)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, p string) (int, error) {
	ctx := r.Context()
	info, err := h.p.Stat(ctx, p)
	if err != nil {
		return 0, err
	}
	if info.IsDir() {
		return h.listing(w, r, p)
	}
	f, err := h.p.Open(ctx, p, simplefs.OpenOptions{Read: true})
	if err != nil {
		return 0, err
	}
	defer f.Close()
	w.Header().Set("ETag", etag(info))
	if ct := mime.TypeByExtension(path.Ext(p)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// ServeContent handles Range, If-Range, If-Modified-Since, If-None-Match
	// and HEAD.
	http.ServeContent(w, r, info.Name(), info.ModTime, io.NewSectionReader(f, 0, info.Size))
	return 0, nil
}

// listing renders a minimal HTML index, for browsers.
func (h *Handler) listing(w http.ResponseWriter, r *http.Request, p string) (int, error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		return http.StatusOK, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<!doctype html><title>%s</title><ul>", html.EscapeString(p))
	for e, err := range simplefs.Entries(r.Context(), h.p, p) {
		if err != nil {
			return 0, err
		}
		name := e.Name
		if e.IsDir() {
			name += "/"
		}
		fmt.Fprintf(&b, `<li><a href="%s">%s</a>`, html.EscapeString(hrefOf(simplefs.JoinPath(p, e.Name), e.IsDir())), html.EscapeString(name))
	}
	b.WriteString("</ul>")
	io.WriteString(w, b.String())
	return 0, nil
}

// copyChunk is the PUT streaming chunk size.
const copyChunk = 1 << 20

func (h *Handler) put(w http.ResponseWriter, r *http.Request, p string) (int, error) {
	ctx := r.Context()
	if p == "/" {
		return http.StatusMethodNotAllowed, nil
	}
	if parent, err := h.p.Stat(ctx, simplefs.ParentPath(p)); err != nil || !parent.IsDir() {
		return http.StatusConflict, nil
	}
	existed, err := simplefs.Exists(ctx, h.p, p)
	if err != nil {
		return 0, err
	}
	f, err := simplefs.Create(ctx, h.p, p, 0)
	if err != nil {
		return 0, err
	}
	buf := make([]byte, copyChunk)
	var off int64
	for {
		n, rerr := io.ReadFull(r.Body, buf)
		if n > 0 {
			if _, err := f.WriteAt(buf[:n], off); err != nil {
				f.Close()
				return 0, err
			}
			off += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			f.Close()
			return http.StatusBadRequest, nil
		}
	}
	if err := f.Sync(ctx); err != nil {
		f.Close()
		return 0, err
	}
	if info, err := f.Stat(ctx); err == nil {
		w.Header().Set("ETag", etag(info))
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if existed {
		return http.StatusNoContent, nil
	}
	return http.StatusCreated, nil
}

func (h *Handler) delete(r *http.Request, p string) (int, error) {
	if p == "/" {
		return http.StatusForbidden, nil
	}
	if _, err := h.p.Stat(r.Context(), p); err != nil {
		return 0, err
	}
	if err := simplefs.RemoveAll(r.Context(), h.p, p); err != nil {
		return 0, err
	}
	return http.StatusNoContent, nil
}

func (h *Handler) mkcol(r *http.Request, p string) (int, error) {
	if r.ContentLength > 0 {
		return http.StatusUnsupportedMediaType, nil
	}
	if ok, err := simplefs.Exists(r.Context(), h.p, p); err != nil {
		return 0, err
	} else if ok {
		return http.StatusMethodNotAllowed, nil
	}
	err := h.p.Mkdir(r.Context(), p, simplefs.MkdirOptions{})
	if errors.Is(err, simplefs.ErrNotExist) || errors.Is(err, simplefs.ErrNotDir) {
		return http.StatusConflict, nil
	}
	if err != nil {
		return 0, err
	}
	return http.StatusCreated, nil
}

func (h *Handler) copyMove(r *http.Request, src string) (int, error) {
	ctx := r.Context()
	dst, err := destination(r)
	if err != nil {
		return http.StatusBadRequest, nil
	}
	if src == "/" || dst == "/" || simplefs.IsUnder(src, dst) || simplefs.IsUnder(dst, src) {
		return http.StatusForbidden, nil
	}
	if _, err := h.p.Stat(ctx, src); err != nil {
		return 0, err
	}
	if parent, err := h.p.Stat(ctx, simplefs.ParentPath(dst)); err != nil || !parent.IsDir() {
		return http.StatusConflict, nil
	}
	overwrite := r.Header.Get("Overwrite") != "F"
	existed, err := simplefs.Exists(ctx, h.p, dst)
	if err != nil {
		return 0, err
	}
	if existed {
		if !overwrite {
			return http.StatusPreconditionFailed, nil
		}
		// RFC 4918: the destination is deleted first, whatever its type.
		if err := simplefs.RemoveAll(ctx, h.p, dst); err != nil {
			return 0, err
		}
	}
	if r.Method == "MOVE" {
		err = h.p.Rename(ctx, src, dst, simplefs.RenameOptions{})
	} else {
		err = h.copyTree(ctx, src, dst, r.Header.Get("Depth") == "0")
	}
	if err != nil {
		return 0, err
	}
	if existed {
		return http.StatusNoContent, nil
	}
	return http.StatusCreated, nil
}

// destination parses the Destination header into a provider path.
func destination(r *http.Request) (string, error) {
	u, err := url.Parse(r.Header.Get("Destination"))
	if err != nil || u.Path == "" {
		return "", errors.New("bad Destination")
	}
	if u.Host != "" && u.Host != r.Host {
		return "", errors.New("Destination on another server")
	}
	return simplefs.CleanPath(u.Path), nil
}

func (h *Handler) copyTree(ctx context.Context, src, dst string, shallow bool) error {
	return simplefs.WalkDir(ctx, h.p, src, func(p string, d simplefs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := dst + strings.TrimPrefix(p, src)
		switch {
		case d.IsDir():
			if err := h.p.Mkdir(ctx, target, simplefs.MkdirOptions{}); err != nil {
				return err
			}
			if shallow { // Depth: 0 copies the collection, not its members
				return fs.SkipDir
			}
		case d.Type.IsSymlink():
			if sp, ok := simplefs.As[simplefs.SymlinkProvider](h.p); ok {
				t, err := sp.Readlink(ctx, p)
				if err != nil {
					return err
				}
				return sp.Symlink(ctx, t, target)
			}
		default:
			_, err := simplefs.Copy(ctx, h.p, target, h.p, p)
			return err
		}
		return nil
	})
}

// Lock stubs.

func (h *Handler) lock(w http.ResponseWriter, r *http.Request, p string) (int, error) {
	ctx := r.Context()
	status := http.StatusOK
	if _, err := h.p.Stat(ctx, p); errors.Is(err, simplefs.ErrNotExist) {
		// Locking an unmapped URL creates an empty resource (RFC 4918 7.3);
		// Office and Finder rely on it.
		f, err := h.p.Open(ctx, p, simplefs.OpenOptions{Write: true, Create: true})
		if err != nil {
			return 0, err
		}
		f.Close()
		status = http.StatusCreated
	} else if err != nil {
		return 0, err
	}
	token := r.Header.Get("If")
	if i, j := strings.Index(token, "(<"), strings.Index(token, ">)"); i >= 0 && j > i {
		token = token[i+2 : j] // a refresh: reuse the token
	} else {
		var b [16]byte
		rand.Read(b[:])
		token = "opaquelocktoken:" + hex.EncodeToString(b[:])
	}
	timeout := r.Header.Get("Timeout")
	if timeout == "" {
		timeout = "Second-3600"
	}
	w.Header().Set("Lock-Token", "<"+token+">")
	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<D:prop xmlns:D="DAV:"><D:lockdiscovery><D:activelock>
<D:locktype><D:write/></D:locktype><D:lockscope><D:exclusive/></D:lockscope>
<D:depth>%s</D:depth><D:timeout>%s</D:timeout>
<D:locktoken><D:href>%s</D:href></D:locktoken>
<D:lockroot><D:href>%s</D:href></D:lockroot>
</D:activelock></D:lockdiscovery></D:prop>`,
		xmlEscape(depthOr(r, "infinity")), xmlEscape(timeout), xmlEscape(token), xmlEscape(hrefOf(p, false)))
	return 0, nil
}

func depthOr(r *http.Request, def string) string {
	if d := r.Header.Get("Depth"); d != "" {
		return d
	}
	return def
}

// hrefOf URL-escapes a path, adding a trailing slash for collections.
func hrefOf(p string, dir bool) string {
	u := (&url.URL{Path: p}).EscapedPath()
	if dir && !strings.HasSuffix(u, "/") {
		u += "/"
	}
	return u
}

func xmlEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// httpTime formats t for getlastmodified.
func httpTime(t time.Time) string { return t.UTC().Format(http.TimeFormat) }
