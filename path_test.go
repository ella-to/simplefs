package simplefs

import (
	"errors"
	"strings"
	"testing"
)

func TestCleanPath(t *testing.T) {
	cases := map[string]string{
		"":          "/",
		"/":         "/",
		"a":         "/a",
		"/a/":       "/a",
		"/a/./b":    "/a/b",
		"/a/../b":   "/b",
		"/../..":    "/",
		"//a//b//":  "/a/b",
		"a/b/../..": "/",
	}
	for in, want := range cases {
		if got := CleanPath(in); got != want {
			t.Errorf("CleanPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPathHelpers(t *testing.T) {
	if got := BaseName("/a/b"); got != "b" {
		t.Errorf("BaseName = %q", got)
	}
	if got := BaseName("/"); got != "/" {
		t.Errorf("BaseName(root) = %q", got)
	}
	if got := ParentPath("/a/b"); got != "/a" {
		t.Errorf("ParentPath = %q", got)
	}
	if got := ParentPath("/"); got != "/" {
		t.Errorf("ParentPath(root) = %q", got)
	}
	if got := JoinPath("/a", "b", "../c"); got != "/a/c" {
		t.Errorf("JoinPath = %q", got)
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"a", "a b", ".hidden", "ü", strings.Repeat("x", MaxNameLength)} {
		if err := ValidName(ok); err != nil {
			t.Errorf("ValidName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", "a\x00b"} {
		if err := ValidName(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidName(%q) = %v, want ErrInvalid", bad, err)
		}
	}
	if err := ValidName(strings.Repeat("x", MaxNameLength+1)); !errors.Is(err, ErrNameTooLong) {
		t.Errorf("long name = %v, want ErrNameTooLong", err)
	}
}

func TestIsUnder(t *testing.T) {
	cases := []struct {
		dir, p string
		want   bool
	}{
		{"/", "/x", true},
		{"/a", "/a", true},
		{"/a", "/a/b", true},
		{"/a/", "a/b/c", true},
		{"/a", "/ab", false},
		{"/a/b", "/a", false},
		{"/a", "/", false},
	}
	for _, c := range cases {
		if got := IsUnder(c.dir, c.p); got != c.want {
			t.Errorf("IsUnder(%q, %q) = %v, want %v", c.dir, c.p, got, c.want)
		}
	}
}
