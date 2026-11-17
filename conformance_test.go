package simplefs_test

import (
	"testing"

	"ella.to/simplefs"
	"ella.to/simplefs/memfs"
	"ella.to/simplefs/providertest"
)

// populated returns a memfs with a small tree, for read-only views.
func populated(t *testing.T) *memfs.FS {
	t.Helper()
	ctx := t.Context()
	p := memfs.New()
	simplefs.MkdirAll(ctx, p, "/dir/nested", 0)
	simplefs.WriteFile(ctx, p, "/file.txt", []byte("hello"), 0)
	simplefs.WriteFile(ctx, p, "/dir/nested/deep.txt", []byte("deep"), 0)
	simplefs.WriteFile(ctx, p, "/empty", nil, 0)
	return p
}

func TestConformanceReadOnly(t *testing.T) {
	providertest.Run(t, func(t *testing.T) simplefs.FileProvider {
		return simplefs.ReadOnly(populated(t))
	}, providertest.Options{ReadOnly: true})
}

func TestConformanceSub(t *testing.T) {
	providertest.Run(t, func(t *testing.T) simplefs.FileProvider {
		p := memfs.New()
		if err := simplefs.MkdirAll(t.Context(), p, "/a/b", 0); err != nil {
			t.Fatal(err)
		}
		return simplefs.Sub(p, "/a/b")
	}, providertest.Options{})
}

func TestConformanceFromFS(t *testing.T) {
	providertest.Run(t, func(t *testing.T) simplefs.FileProvider {
		return simplefs.FromFS(simplefs.FS(t.Context(), populated(t)))
	}, providertest.Options{ReadOnly: true})
}
