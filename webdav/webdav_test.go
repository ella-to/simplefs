package webdav_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"ella.to/simplefs"
	"ella.to/simplefs/memfs"
	"ella.to/simplefs/webdav"
)

type client struct {
	t   *testing.T
	url string
}

func newServer(t *testing.T, p simplefs.FileProvider, opts webdav.Options) *client {
	t.Helper()
	srv := httptest.NewServer(webdav.NewHandler(p, opts))
	t.Cleanup(srv.Close)
	return &client{t: t, url: srv.URL}
}

func (c *client) do(method, path, body string, hdr ...string) (*http.Response, string) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.url+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (c *client) want(method, path, body string, status int, hdr ...string) string {
	c.t.Helper()
	resp, out := c.do(method, path, body, hdr...)
	if resp.StatusCode != status {
		c.t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, status, out)
	}
	return out
}

func TestCRUD(t *testing.T) {
	p := memfs.New()
	c := newServer(t, p, webdav.Options{})
	ctx := t.Context()

	c.want("OPTIONS", "/", "", 200)
	c.want("MKCOL", "/docs", "", 201)
	c.want("MKCOL", "/docs", "", 405)
	c.want("MKCOL", "/missing/child", "", 409)
	c.want("PUT", "/docs/a.txt", "hello webdav", 201)
	c.want("PUT", "/docs/a.txt", "hello again!", 204)
	c.want("PUT", "/nodir/a.txt", "x", 409)
	if got, _ := simplefs.ReadFile(ctx, p, "/docs/a.txt"); string(got) != "hello again!" {
		t.Fatalf("provider content = %q", got)
	}

	if got := c.want("GET", "/docs/a.txt", "", 200); got != "hello again!" {
		t.Fatalf("GET = %q", got)
	}
	if got := c.want("GET", "/docs/a.txt", "", 206, "Range", "bytes=6-10"); got != "again" {
		t.Fatalf("ranged GET = %q", got)
	}
	resp, _ := c.do("HEAD", "/docs/a.txt", "")
	if resp.Header.Get("ETag") == "" || resp.ContentLength != 12 {
		t.Fatalf("HEAD: etag %q length %d", resp.Header.Get("ETag"), resp.ContentLength)
	}
	c.want("GET", "/docs/a.txt", "", 304, "If-None-Match", resp.Header.Get("ETag"))
	c.want("GET", "/nope", "", 404)

	c.want("COPY", "/docs/a.txt", "", 201, "Destination", c.url+"/docs/b.txt")
	c.want("MOVE", "/docs/b.txt", "", 201, "Destination", "/docs/c.txt")
	c.want("MOVE", "/docs/a.txt", "", 412, "Destination", "/docs/c.txt", "Overwrite", "F")
	c.want("MOVE", "/docs/a.txt", "", 204, "Destination", "/docs/c.txt")
	if got, _ := simplefs.ReadFile(ctx, p, "/docs/c.txt"); string(got) != "hello again!" {
		t.Fatalf("after MOVE = %q", got)
	}
	c.want("COPY", "/docs", "", 201, "Destination", "/copy")
	if ok, _ := simplefs.Exists(ctx, p, "/copy/c.txt"); !ok {
		t.Fatal("COPY of a collection did not copy members")
	}
	c.want("COPY", "/docs", "", 403, "Destination", "/docs/inside")

	c.want("DELETE", "/docs", "", 204)
	c.want("DELETE", "/docs", "", 404)
}

func TestPropfind(t *testing.T) {
	p := memfs.New()
	simplefs.MkdirAll(t.Context(), p, "/dir/sub", 0)
	simplefs.WriteFile(t.Context(), p, "/dir/file name.txt", []byte("12345"), 0)
	c := newServer(t, p, webdav.Options{})

	out := c.want("PROPFIND", "/dir", "", 207, "Depth", "1")
	for _, want := range []string{
		"<D:href>/dir/</D:href>",
		"<D:href>/dir/file%20name.txt</D:href>",
		"<D:href>/dir/sub/</D:href>",
		"<D:getcontentlength>5</D:getcontentlength>",
		"<D:collection/>",
		"<D:quota-available-bytes>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("PROPFIND missing %s in:\n%s", want, out)
		}
	}
	out = c.want("PROPFIND", "/dir/file name.txt", `<?xml version="1.0"?>
<D:propfind xmlns:D="DAV:"><D:prop><D:getcontentlength/><D:nonsense/></D:prop></D:propfind>`, 207, "Depth", "0")
	if !strings.Contains(out, "<D:getcontentlength>5") || !strings.Contains(out, "404 Not Found") {
		t.Fatalf("PROPFIND with prop list:\n%s", out)
	}
	if strings.Contains(out, "getlastmodified") {
		t.Fatal("PROPFIND returned a property that was not requested")
	}
	c.want("PROPFIND", "/missing", "", 404, "Depth", "0")
}

func TestProppatchSetsModTime(t *testing.T) {
	p := memfs.New()
	simplefs.WriteFile(t.Context(), p, "/f", nil, 0)
	c := newServer(t, p, webdav.Options{})
	when := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	out := c.want("PROPPATCH", "/f", `<?xml version="1.0"?>
<D:propertyupdate xmlns:D="DAV:" xmlns:Z="urn:schemas-microsoft-com:"><D:set><D:prop>
<Z:Win32LastModifiedTime>`+when.Format(http.TimeFormat)+`</Z:Win32LastModifiedTime>
<Z:Win32FileAttributes>00000020</Z:Win32FileAttributes>
</D:prop></D:set></D:propertyupdate>`, 207)
	if !strings.Contains(out, "Win32FileAttributes") || !strings.Contains(out, "200 OK") {
		t.Fatalf("PROPPATCH response:\n%s", out)
	}
	info, _ := p.Stat(t.Context(), "/f")
	if !info.ModTime.Equal(when) {
		t.Fatalf("ModTime = %v, want %v", info.ModTime, when)
	}
}

func TestLocks(t *testing.T) {
	p := memfs.New()
	c := newServer(t, p, webdav.Options{})
	resp, _ := c.do("LOCK", "/new.docx", `<?xml version="1.0"?>
<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`)
	if resp.StatusCode != 201 || !strings.HasPrefix(resp.Header.Get("Lock-Token"), "<opaquelocktoken:") {
		t.Fatalf("LOCK unmapped = %d, token %q", resp.StatusCode, resp.Header.Get("Lock-Token"))
	}
	if ok, _ := simplefs.Exists(t.Context(), p, "/new.docx"); !ok {
		t.Fatal("LOCK on an unmapped URL did not create the resource")
	}
	token := strings.Trim(resp.Header.Get("Lock-Token"), "<>")
	resp, _ = c.do("LOCK", "/new.docx", "", "If", "(<"+token+">)")
	if resp.StatusCode != 200 || resp.Header.Get("Lock-Token") != "<"+token+">" {
		t.Fatalf("LOCK refresh = %d, %q", resp.StatusCode, resp.Header.Get("Lock-Token"))
	}
	c.want("UNLOCK", "/new.docx", "", 204, "Lock-Token", "<"+token+">")
}

func TestReadOnlyAndClients(t *testing.T) {
	p := memfs.New()
	simplefs.WriteFile(t.Context(), p, "/f", []byte("x"), 0)
	c := newServer(t, p, webdav.Options{ReadOnly: true})
	c.want("GET", "/f", "", 200)
	c.want("PUT", "/f", "y", 403)
	c.want("DELETE", "/f", "", 403)
	c.want("MKCOL", "/d", "", 403)

	only := newServer(t, p, webdav.Options{AllowedClients: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}})
	only.want("GET", "/f", "", 403)
}
