package simplefs

import (
	"fmt"
	"path"
	"strings"
)

// CleanPath normalizes a logical path: forward slashes only, rooted at "/",
// "." and ".." segments resolved, no trailing slash (except the root itself).
// The empty string normalizes to "/".
//
// Providers should treat CleanPath(p) as the canonical identity of p — every
// FileInfo.Path they return must already be in this form.
func CleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

// BaseName returns the last element of the cleaned path; the root's base name
// is "/".
func BaseName(p string) string {
	return path.Base(CleanPath(p))
}

// ParentPath returns the cleaned path's parent directory; the root's parent
// is the root itself.
func ParentPath(p string) string {
	return path.Dir(CleanPath(p))
}

// JoinPath joins elements onto a cleaned base path.
func JoinPath(base string, elem ...string) string {
	return CleanPath(path.Join(append([]string{CleanPath(base)}, elem...)...))
}

// MaxNameLength is the longest path element ValidName accepts, in bytes.
const MaxNameLength = 255

// ValidName reports whether name can be a single path element: it must be
// non-empty, not "." or "..", contain no '/' or NUL byte, and be at most
// MaxNameLength bytes. The error wraps ErrNameTooLong or ErrInvalid.
func ValidName(name string) error {
	switch {
	case name == "" || name == "." || name == "..":
		return fmt.Errorf("%w: name %q", ErrInvalid, name)
	case strings.ContainsAny(name, "/\x00"):
		return fmt.Errorf("%w: name %q contains '/' or NUL", ErrInvalid, name)
	case len(name) > MaxNameLength:
		return fmt.Errorf("%w: %d bytes", ErrNameTooLong, len(name))
	}
	return nil
}

// IsUnder reports whether p is dir itself or lies inside it. Both paths are
// cleaned first, so IsUnder("/a", "/a/b") and IsUnder("a/", "/a") are true
// and IsUnder("/a", "/ab") is false.
func IsUnder(dir, p string) bool {
	dir, p = CleanPath(dir), CleanPath(p)
	if dir == "/" || dir == p {
		return true
	}
	return len(p) > len(dir) && p[len(dir)] == '/' && p[:len(dir)] == dir
}
