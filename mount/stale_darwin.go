package mount

import (
	"bufio"
	"context"
	"os/exec"
	"strings"
)

// loopbackMounts parses `nfsstat -m` for NFSv3 mounts of 127.0.0.1:/ that
// carry an explicit port. (`mount` hides nobrowse mounts, nfsstat does not.)
func loopbackMounts(ctx context.Context) ([]staleMount, error) {
	out, err := exec.CommandContext(ctx, "/usr/bin/nfsstat", "-m").Output()
	if err != nil {
		return nil, err
	}
	return parseNfsstat(bufio.NewScanner(strings.NewReader(string(out)))), nil
}

func parseNfsstat(sc *bufio.Scanner) []staleMount {
	// /path/to/mnt from 127.0.0.1:/
	//   -- Original mount options:
	//      NFS parameters: vers=3,tcp,port=61179,mountport=61179,...
	var out []staleMount
	var path string
	for sc.Scan() {
		line := sc.Text()
		if p, ok := strings.CutSuffix(line, " from 127.0.0.1:/"); ok && !strings.HasPrefix(line, " ") {
			path = p
			continue
		}
		if !strings.HasPrefix(line, " ") && line != "" {
			path = "" // a mount of something else
			continue
		}
		params, ok := strings.CutPrefix(strings.TrimSpace(line), "NFS parameters: ")
		if !ok || path == "" {
			continue
		}
		if strings.HasPrefix(params, "vers=3,") {
			if port, ok := optPort(params); ok {
				out = append(out, staleMount{path: path, port: port})
			}
		}
		path = "" // only the first (original) parameter line counts
	}
	return out
}
