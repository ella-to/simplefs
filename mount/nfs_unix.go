//go:build !windows

package mount

import (
	"ella.to/simplefs"
	"ella.to/simplefs/nfs"
)

// serveNFS starts a dedicated loopback server for one mount, on an
// ephemeral port — the client is told the port explicitly, so no portmapper
// is involved.
func serveNFS(p simplefs.FileProvider, o nfs.Options, name string) (served, error) {
	srv := nfs.NewServer(p, o)
	if err := srv.Start("127.0.0.1:0"); err != nil {
		return served{}, err
	}
	return served{port: srv.Port(), export: "/", stop: srv.Shutdown}, nil
}

func autoBackend() Backend { return NFS }
