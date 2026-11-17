package webdav

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"ella.to/simplefs"
)

// propfindReq is the subset of a PROPFIND body the handler needs: whether
// specific properties were asked for. Unknown requested properties are
// reported 404; with allprop (or an empty body) every live property is.
type propfindReq struct {
	XMLName  xml.Name  `xml:"DAV: propfind"`
	PropName *struct{} `xml:"DAV: propname"`
	Prop     *struct {
		Any []xml.Name `xml:",any"`
	} `xml:"DAV: prop"`
}

// liveProps are the properties served for every resource.
var liveProps = []xml.Name{
	{Space: "DAV:", Local: "displayname"},
	{Space: "DAV:", Local: "resourcetype"},
	{Space: "DAV:", Local: "getcontentlength"},
	{Space: "DAV:", Local: "getcontenttype"},
	{Space: "DAV:", Local: "getlastmodified"},
	{Space: "DAV:", Local: "creationdate"},
	{Space: "DAV:", Local: "getetag"},
	{Space: "DAV:", Local: "supportedlock"},
	{Space: "DAV:", Local: "lockdiscovery"},
	{Space: "DAV:", Local: "quota-available-bytes"},
	{Space: "DAV:", Local: "quota-used-bytes"},
}

func (h *Handler) propfind(w http.ResponseWriter, r *http.Request, p string) (int, error) {
	ctx := r.Context()
	var req propfindReq
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return http.StatusBadRequest, nil
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := xml.Unmarshal(body, &req); err != nil {
			return http.StatusBadRequest, nil
		}
	}
	depth := depthOr(r, "infinity")
	if depth != "0" && depth != "1" {
		// Infinite depth would walk the whole provider; RFC 4918 lets
		// servers refuse it.
		depth = "1"
	}

	info, err := h.p.Stat(ctx, p)
	if err != nil {
		return 0, err
	}
	var quota *simplefs.FSInfo
	if sp, ok := simplefs.As[simplefs.StatFSProvider](h.p); ok {
		if q, err := sp.StatFS(ctx); err == nil {
			quota = &q
		}
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n" + `<D:multistatus xmlns:D="DAV:">`)
	h.writeResponse(&b, info, &req, quota)
	if depth == "1" && info.IsDir() {
		for e, err := range simplefs.Entries(ctx, h.p, p) {
			if err != nil {
				return 0, err
			}
			child := simplefs.FileInfo{}
			if e.Info != nil {
				child = *e.Info
				child.Path = simplefs.JoinPath(p, e.Name)
			} else if child, err = h.p.Stat(ctx, simplefs.JoinPath(p, e.Name)); err != nil {
				if errors.Is(err, simplefs.ErrNotExist) {
					continue
				}
				return 0, err
			}
			h.writeResponse(&b, child, &req, quota)
		}
	}
	b.WriteString("</D:multistatus>")
	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusMultiStatus)
	io.WriteString(w, b.String())
	return 0, nil
}

// writeResponse appends the <D:response> for one resource.
func (h *Handler) writeResponse(b *strings.Builder, info simplefs.FileInfo, req *propfindReq, quota *simplefs.FSInfo) {
	fmt.Fprintf(b, "<D:response><D:href>%s</D:href>", xmlEscape(hrefOf(info.Path, info.IsDir())))
	if req.PropName != nil {
		b.WriteString("<D:propstat><D:prop>")
		for _, n := range liveProps {
			fmt.Fprintf(b, "<D:%s/>", n.Local)
		}
		b.WriteString("</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>")
		return
	}
	wanted := liveProps
	if req.Prop != nil {
		wanted = req.Prop.Any
	}
	var found, missing strings.Builder
	for _, n := range wanted {
		if v, ok := propValue(n, info, quota); ok {
			found.WriteString(v)
		} else {
			fmt.Fprintf(&missing, `<%s xmlns="%s"/>`, n.Local, xmlEscape(n.Space))
		}
	}
	if found.Len() > 0 {
		fmt.Fprintf(b, "<D:propstat><D:prop>%s</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>", found.String())
	}
	if missing.Len() > 0 {
		fmt.Fprintf(b, "<D:propstat><D:prop>%s</D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat>", missing.String())
	}
	b.WriteString("</D:response>")
}

// propValue renders one live property, or reports that it does not apply.
func propValue(n xml.Name, info simplefs.FileInfo, quota *simplefs.FSInfo) (string, bool) {
	if n.Space != "DAV:" {
		return "", false
	}
	switch n.Local {
	case "displayname":
		name := info.Name()
		if info.Path == "/" {
			name = ""
		}
		return "<D:displayname>" + xmlEscape(name) + "</D:displayname>", true
	case "resourcetype":
		if info.IsDir() {
			return "<D:resourcetype><D:collection/></D:resourcetype>", true
		}
		return "<D:resourcetype/>", true
	case "getcontentlength":
		if info.IsDir() {
			return "", false
		}
		return fmt.Sprintf("<D:getcontentlength>%d</D:getcontentlength>", info.Size), true
	case "getcontenttype":
		if info.IsDir() {
			return "", false
		}
		ct := mime.TypeByExtension(path.Ext(info.Path))
		if ct == "" {
			ct = "application/octet-stream"
		}
		return "<D:getcontenttype>" + xmlEscape(ct) + "</D:getcontenttype>", true
	case "getlastmodified":
		return "<D:getlastmodified>" + httpTime(info.ModTime) + "</D:getlastmodified>", true
	case "creationdate":
		return "<D:creationdate>" + info.ModTime.UTC().Format(time.RFC3339) + "</D:creationdate>", true
	case "getetag":
		if info.IsDir() {
			return "", false
		}
		return "<D:getetag>" + xmlEscape(etag(info)) + "</D:getetag>", true
	case "supportedlock":
		return "<D:supportedlock><D:lockentry><D:lockscope><D:exclusive/></D:lockscope>" +
			"<D:locktype><D:write/></D:locktype></D:lockentry></D:supportedlock>", true
	case "lockdiscovery":
		return "<D:lockdiscovery/>", true
	case "quota-available-bytes":
		if quota == nil || quota.BlockSize == 0 {
			return "", false
		}
		return fmt.Sprintf("<D:quota-available-bytes>%d</D:quota-available-bytes>", quota.Available*quota.BlockSize), true
	case "quota-used-bytes":
		if quota == nil || quota.BlockSize == 0 {
			return "", false
		}
		return fmt.Sprintf("<D:quota-used-bytes>%d</D:quota-used-bytes>", (quota.Blocks-quota.Free)*quota.BlockSize), true
	}
	return "", false
}

// proppatchReq captures the properties a PROPPATCH sets or removes.
type proppatchReq struct {
	XMLName xml.Name `xml:"DAV: propertyupdate"`
	Set     []struct {
		Prop struct {
			Any []struct {
				XMLName xml.Name
				Value   string `xml:",chardata"`
			} `xml:",any"`
		} `xml:"DAV: prop"`
	} `xml:"DAV: set"`
	Remove []struct {
		Prop struct {
			Any []struct {
				XMLName xml.Name
			} `xml:",any"`
		} `xml:"DAV: prop"`
	} `xml:"DAV: remove"`
}

// proppatch applies modification times (DAV:getlastmodified, or Windows'
// Win32LastModifiedTime) through ChtimesProvider and reports every other
// property as set: clients such as Windows set Win32FileAttributes and the
// like on every write and give up if that fails, but there is nowhere to
// store them.
func (h *Handler) proppatch(w http.ResponseWriter, r *http.Request, p string) (int, error) {
	ctx := r.Context()
	if _, err := h.p.Stat(ctx, p); err != nil {
		return 0, err
	}
	if h.opts.ReadOnly {
		return http.StatusForbidden, nil
	}
	var req proppatchReq
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || xml.Unmarshal(body, &req) != nil {
		return http.StatusBadRequest, nil
	}
	var names []xml.Name
	for _, set := range req.Set {
		for _, prop := range set.Prop.Any {
			names = append(names, prop.XMLName)
			if prop.XMLName.Local != "getlastmodified" && prop.XMLName.Local != "Win32LastModifiedTime" {
				continue
			}
			t, err := http.ParseTime(strings.TrimSpace(prop.Value))
			if err != nil {
				continue
			}
			if cp, ok := simplefs.As[simplefs.ChtimesProvider](h.p); ok {
				if err := cp.Chtimes(ctx, p, time.Time{}, t); err != nil && !errors.Is(err, simplefs.ErrNotSupported) {
					return 0, err
				}
			}
		}
	}
	for _, rm := range req.Remove {
		for _, prop := range rm.Prop.Any {
			names = append(names, prop.XMLName)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="utf-8"?>`+"\n"+`<D:multistatus xmlns:D="DAV:"><D:response><D:href>%s</D:href><D:propstat><D:prop>`,
		xmlEscape(hrefOf(p, false)))
	for _, n := range names {
		fmt.Fprintf(&b, `<%s xmlns="%s"/>`, n.Local, xmlEscape(n.Space))
	}
	b.WriteString("</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>")
	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusMultiStatus)
	io.WriteString(w, b.String())
	return 0, nil
}
