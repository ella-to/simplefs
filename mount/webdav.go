package mount

import (
	"context"
	"fmt"

	"ella.to/simplefs"
)

func mountWebDAV(ctx context.Context, p simplefs.FileProvider, opts Options) (*Point, error) {
	return nil, fmt.Errorf("%w: the WebDAV backend is not implemented yet", ErrUnsupported)
}
