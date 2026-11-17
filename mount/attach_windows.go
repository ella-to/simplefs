package mount

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"

	"ella.to/simplefs/nfs"
)

// nfsPlatform: mount.exe always talks to port 2049 and discovers the MOUNT
// program through the portmapper on 111, so neither port is configurable.
// Windows lets unprivileged processes bind low ports. Until the server can
// export several providers, one process (and one machine) can therefore
// hold a single NFS mount at a time.
func nfsPlatform(o *nfs.Options) string {
	o.Portmap = true
	return "127.0.0.1:2049"
}

// autoBackend prefers NFS, and falls back to WebDAV on editions without
// Client for NFS (Windows Home).
func autoBackend() Backend {
	if _, err := exec.LookPath("mount.exe"); err == nil {
		return NFS
	}
	return WebDAV
}

var procGetLogicalDrives = syscall.NewLazyDLL("kernel32.dll").NewProc("GetLogicalDrives")

// freeDrive returns the highest unused drive letter, D: through Z:.
func freeDrive() (string, error) {
	mask, _, err := procGetLogicalDrives.Call()
	if mask == 0 {
		return "", fmt.Errorf("mount: GetLogicalDrives: %w", err)
	}
	for l := 'Z'; l >= 'D'; l-- {
		if mask&(1<<uint(l-'A')) == 0 {
			return string(l) + ":", nil
		}
	}
	return "", fmt.Errorf("mount: no free drive letter")
}

// drive normalizes "z", "Z:", `Z:\` to "Z:".
func drive(p string) (string, error) {
	l := strings.ToUpper(strings.TrimSuffix(strings.TrimSuffix(p, `\`), ":"))
	if len(l) != 1 || l[0] < 'A' || l[0] > 'Z' {
		return "", fmt.Errorf("mount: Options.Path %q is not a drive letter (e.g. \"Z:\")", p)
	}
	return l + ":", nil
}

// attachNFS maps the loopback server to a drive letter with the built-in
// Client for NFS. The UNC share name is arbitrary: the server exports a
// single root.
func attachNFS(ctx context.Context, cfg attachConfig) (string, func(), error) {
	if _, err := exec.LookPath("mount.exe"); err != nil {
		return "", nil, fmt.Errorf("%w: mount.exe not found — enable \"Client for NFS\" "+
			"(PowerShell as admin: Enable-WindowsOptionalFeature -Online -FeatureName "+
			"ServicesForNFS-ClientOnly,ClientForNFS-Infrastructure -All), or use Backend: WebDAV",
			ErrNoClient)
	}
	letter := cfg.path
	var err error
	if letter == "" {
		letter, err = freeDrive()
	} else {
		letter, err = drive(letter)
	}
	if err != nil {
		return "", nil, err
	}
	opts := "anon,nolock,mtype=soft,timeout=5,retry=1"
	if cfg.readOnly {
		opts += ",fileaccess=444"
	}
	out, err := exec.CommandContext(ctx, "mount.exe", "-o", opts, `\\127.0.0.1\`+cfg.name, letter).CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("mount: mount.exe %s: %s: %w", letter, strings.TrimSpace(string(out)), err)
	}
	return letter + `\`, func() {}, nil
}

// detachNFS unmaps the drive letter.
func detachNFS(target string) error {
	letter := strings.TrimSuffix(target, `\`)
	out, err := exec.Command("umount.exe", "-f", letter).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount: umount.exe %s: %s: %w", letter, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// CleanupStale is a no-op on Windows: a mapped drive whose server is gone
// simply reports errors, and Windows forgets it at logoff.
func CleanupStale(ctx context.Context) error { return nil }
