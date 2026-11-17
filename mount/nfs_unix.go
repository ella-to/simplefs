//go:build !windows

package mount

import "ella.to/simplefs/nfs"

// nfsPlatform adjusts the server options and returns the listen address:
// loopback on an ephemeral port — the client is told the port explicitly,
// so no portmapper is involved.
func nfsPlatform(*nfs.Options) string { return "127.0.0.1:0" }

func autoBackend() Backend { return NFS }
