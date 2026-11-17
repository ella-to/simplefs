package dirfs_test

import (
	"runtime"
	"testing"

	"ella.to/simplefs"
	"ella.to/simplefs/dirfs"
	"ella.to/simplefs/providertest"
)

func TestConformance(t *testing.T) {
	providertest.Run(t, func(t *testing.T) simplefs.FileProvider {
		p, err := dirfs.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Close() })
		return p
	}, providertest.Options{
		// APFS and NTFS are case-insensitive by default; APFS also treats
		// NFC and NFD spellings as the same name.
		CaseInsensitive:   runtime.GOOS == "darwin" || runtime.GOOS == "windows",
		NormalizesUnicode: runtime.GOOS == "darwin",
	})
}
