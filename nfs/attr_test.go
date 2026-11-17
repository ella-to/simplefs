package nfs

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"ella.to/simplefs"
)

func TestStatusOf(t *testing.T) {
	wrap := func(err error) error { return &simplefs.PathError{Op: "op", Path: "/p", Err: err} }
	cases := []struct {
		err  error
		want uint32
	}{
		{nil, nfsOK},
		{simplefs.ErrNotExist, nfsErrNoEnt},
		{simplefs.ErrExist, nfsErrExist},
		{simplefs.ErrNotEmpty, nfsErrNotEmpty},
		{simplefs.ErrIsDir, nfsErrIsDir},
		{simplefs.ErrNotDir, nfsErrNotDir},
		{simplefs.ErrPermission, nfsErrAcces},
		{simplefs.ErrReadOnly, nfsErrROFS},
		{simplefs.ErrNoSpace, nfsErrNoSpc},
		{simplefs.ErrTooLarge, nfsErrFBig},
		{simplefs.ErrNameTooLong, nfsErrNameTooL},
		{simplefs.ErrNotSupported, nfsErrNotSupp},
		{simplefs.ErrInvalid, nfsErrInval},
		{simplefs.ErrBusy, nfsErrJukebox},
		{context.DeadlineExceeded, nfsErrJukebox},
		{errors.New("boom"), nfsErrIO},
	}
	for _, c := range cases {
		if got := statusOf(c.err); got != c.want {
			t.Errorf("statusOf(%v) = %d, want %d", c.err, got, c.want)
		}
		if c.err != nil {
			if got := statusOf(wrap(c.err)); got != c.want {
				t.Errorf("statusOf(wrapped %v) = %d, want %d", c.err, got, c.want)
			}
			if got := statusOf(fmt.Errorf("ctx: %w", c.err)); got != c.want {
				t.Errorf("statusOf(fmt-wrapped %v) = %d, want %d", c.err, got, c.want)
			}
		}
	}
}
