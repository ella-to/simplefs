package dirfs

import (
	"runtime"
	"strings"

	"ella.to/simplefs"
)

// checkName validates the final element of a path about to be created. On
// Windows it also rejects names NTFS cannot store, so the error is a clear
// ErrInvalid instead of a confusing OS failure.
func checkName(n string) error {
	if err := simplefs.ValidName(n); err != nil {
		return err
	}
	if runtime.GOOS == "windows" && !validWindowsName(n) {
		return simplefs.ErrInvalid
	}
	return nil
}

// validWindowsName reports whether n is a legal NTFS file name.
func validWindowsName(n string) bool {
	if strings.ContainsAny(n, `<>:"/\|?*`) || strings.HasSuffix(n, ".") || strings.HasSuffix(n, " ") {
		return false
	}
	for _, r := range n {
		if r < 32 {
			return false
		}
	}
	base := strings.ToUpper(n)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	switch base {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return false
	}
	return true
}
