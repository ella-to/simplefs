package simplefs

import (
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
