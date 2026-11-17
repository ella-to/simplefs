package mount

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"ella.to/simplefs/nfs"
)

// nfsOptions: Windows' NFS client authenticates as anonymous (-o anon); the
// server-side owner ids are cosmetic there.
func nfsOptions() nfs.Options {
	// Portmap is required: mount.exe discovers the NFS and MOUNT ports by
	// asking the portmapper on 111. Windows allows non-admin processes to
	// bind low ports, so this normally just works.
	return nfs.Options{Portmap: true}
}

// nfsListenAddr must be the standard NFS port — mount.exe cannot be pointed
// at an arbitrary one.
func nfsListenAddr() string { return "127.0.0.1:2049" }

var driveRe = regexp.MustCompile(`(?m)\b([A-Z]):`)

// nfsAttach maps the loopback server to a drive letter with the built-in
// "Client for NFS" (an optional Windows feature on Pro/Enterprise/Education:
// Control Panel -> Windows Features -> Services for NFS -> Client for NFS).
// The domain ID and port are fixed by mount.exe's constraints: it always
// talks to 2049 via the portmapper, so neither is configurable here.
func nfsAttach(_ string, _ int) (string, error) {
	// "*" lets mount.exe choose a free drive letter; the share component of
	// the UNC path is arbitrary because the server exports a single root.
	out, err := exec.Command("mount.exe", "-o", "anon,nolock", `\\127.0.0.1\simplefs`, "*").CombinedOutput()
	if err != nil {
		if _, lookErr := exec.LookPath("mount.exe"); lookErr != nil {
			return "", fmt.Errorf("mount: mount.exe not found — enable the "+
				"\"Client for NFS\" Windows feature (Pro/Enterprise), or serve with "+
				"nfs.Server and use another NFS client: %w", err)
		}
		return "", fmt.Errorf("mount: mount.exe failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	m := driveRe.FindStringSubmatch(string(out))
	if m == nil {
		return "", fmt.Errorf("mount: cannot find drive letter in mount.exe output: %q", string(out))
	}
	return m[1] + `:\`, nil
}

// nfsDetach unmaps the drive letter.
func nfsDetach(target string) error {
	drive := strings.TrimSuffix(target, `\`)
	out, err := exec.Command("umount.exe", "-f", drive).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount: umount.exe %s: %s: %w", drive, strings.TrimSpace(string(out)), err)
	}
	return nil
}
