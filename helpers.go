package simplefs

import "context"

// Create creates the file at path, or truncates it if it exists, and opens
// it for reading and writing — the equivalent of os.Create. A zero mode
// means 0o644.
func Create(ctx context.Context, p FileProvider, path string, mode FileMode) (FileHandle, error) {
	return p.Open(ctx, path, OpenOptions{Read: true, Write: true, Create: true, Truncate: true, Mode: mode})
}
