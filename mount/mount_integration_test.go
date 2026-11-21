//go:build mounttest

// Integration tests that really mount. They need the platform's NFS client
// and, on Linux and FreeBSD, root:
//
//	go test -tags mounttest ./mount/            # macOS, Windows
//	sudo -E go test -tags mounttest ./mount/    # Linux
package mount_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"ella.to/simplefs"
	"ella.to/simplefs/memfs"
	"ella.to/simplefs/mount"
)

func mountMem(t *testing.T, opts mount.Options) (*memfs.FS, *mount.Point) {
	t.Helper()
	p := memfs.New()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	m, err := mount.Mount(ctx, p, opts)
	if errors.Is(err, mount.ErrNoPrivilege) || errors.Is(err, mount.ErrNoClient) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Unmount(context.Background()); err != nil {
			t.Errorf("Unmount: %v", err)
		}
	})
	return p, m
}

// skipOnWindows skips tests that mount at a directory: Windows mounts only
// at drive letters, and CleanupStale is a no-op there.
func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows mounts at drive letters, not directories")
	}
}

func TestMountFileIO(t *testing.T) {
	p, m := mountMem(t, mount.Options{Name: "iotest"})
	root := m.Path()

	// Through the mount -> visible in the provider.
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := simplefs.ReadFile(t.Context(), p, "/hello.txt")
	if err != nil || string(got) != "hello" {
		t.Fatalf("provider sees %q, %v", got, err)
	}

	// Through the provider -> visible in the mount.
	simplefs.WriteFile(t.Context(), p, "/from-go.txt", []byte("go"), 0)
	if data, err := os.ReadFile(filepath.Join(root, "from-go.txt")); err != nil || string(data) != "go" {
		t.Fatalf("mount sees %q, %v", data, err)
	}

	// Directories, rename, big file, truncate, remove.
	nested := filepath.Join(root, "docs", "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 3<<20)
	rand.Read(big)
	if err := os.WriteFile(filepath.Join(nested, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "docs"), filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "moved", "nested", "big.bin"))
	if err != nil || !bytes.Equal(data, big) {
		t.Fatalf("big file round trip failed: %v", err)
	}
	if err := os.Truncate(filepath.Join(root, "moved", "nested", "big.bin"), 100); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(root, "moved", "nested", "big.bin")); err != nil || fi.Size() != 100 {
		t.Fatalf("after truncate: %v, %v", fi, err)
	}
	if err := os.RemoveAll(filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.Name() != ".DS_Store" && !bytes.HasPrefix([]byte(e.Name()), []byte("._")) {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"from-go.txt", "hello.txt"}) {
		t.Fatalf("root lists %v", names)
	}
}

func TestMountReadOnly(t *testing.T) {
	_, m := mountMem(t, mount.Options{Name: "rotest", ReadOnly: true})
	err := os.WriteFile(filepath.Join(m.Path(), "x"), nil, 0o644)
	if err == nil {
		t.Fatal("write to a read-only mount succeeded")
	}
}

func TestMountExplicitPathAndUnmount(t *testing.T) {
	skipOnWindows(t)
	dir := filepath.Join(t.TempDir(), "mnt")
	_, m := mountMem(t, mount.Options{Name: "pathtest", Path: dir})
	if m.Path() != dir {
		if resolved, _ := filepath.EvalSymlinks(dir); resolved != m.Path() && m.Path() != dir {
			t.Fatalf("Path = %q, want %q", m.Path(), dir)
		}
	}
	if fi, err := os.Lstat(dir); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s is not a real mount directory: %v, %v", dir, fi, err)
	}
	if err := m.Unmount(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.Done():
	default:
		t.Fatal("Done not closed after Unmount")
	}
	if err := m.Unmount(t.Context()); err != nil {
		t.Fatalf("second Unmount = %v", err)
	}
}

// TestMain lets the test binary double as a child process that mounts and
// then blocks, so TestCleanupStaleAfterCrash can kill it.
func TestMain(m *testing.M) {
	if dir := os.Getenv("SIMPLEFS_CRASH_CHILD"); dir != "" {
		mt, err := mount.Mount(context.Background(), memfs.New(), mount.Options{Name: "crash", Path: dir})
		if err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(2)
		}
		os.Stdout.WriteString(mt.Path() + "\n")
		select {} // wait to be killed
	}
	os.Exit(m.Run())
}

func TestCleanupStaleAfterCrash(t *testing.T) {
	skipOnWindows(t)
	dir := filepath.Join(t.TempDir(), "crash")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "SIMPLEFS_CRASH_CHILD="+dir)
	stdout, _ := cmd.StdoutPipe()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		cmd.Wait()
		if strings.Contains(stderr.String(), "privileges") {
			t.Skip(stderr.String())
		}
		t.Fatalf("child did not mount: %v: %s", err, stderr.String())
	}
	mounted := strings.TrimSpace(line)
	cmd.Process.Kill() // crash: no Unmount
	cmd.Wait()

	// The soft mount must fail instead of hanging.
	done := make(chan error, 1)
	go func() { _, err := os.ReadDir(mounted); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Log("ReadDir on the dead mount still succeeded (served from cache)")
		}
	case <-time.After(90 * time.Second):
		t.Fatal("ReadDir on a dead mount hung")
	}

	if err := mount.CleanupStale(t.Context()); err != nil {
		t.Fatalf("CleanupStale: %v", err)
	}
	if out, _ := exec.Command("mount").CombinedOutput(); bytes.Contains(out, []byte(mounted)) {
		t.Fatalf("%s still mounted after CleanupStale", mounted)
	}
	if runtime.GOOS == "darwin" {
		out, _ := exec.Command("nfsstat", "-m").CombinedOutput()
		if bytes.Contains(out, []byte(mounted)) {
			t.Fatalf("%s still mounted after CleanupStale", mounted)
		}
	}
}

func TestMountWebDAV(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("no built-in WebDAV client")
	}
	p, m := mountMem(t, mount.Options{Name: "davtest", Backend: mount.WebDAV})
	if err := os.WriteFile(filepath.Join(m.Path(), "dav.txt"), []byte("over webdav"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := simplefs.ReadFile(t.Context(), p, "/dav.txt"); err != nil || string(got) != "over webdav" {
		t.Fatalf("provider sees %q, %v", got, err)
	}
	if err := os.Mkdir(filepath.Join(m.Path(), "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(m.Path(), "dav.txt"), filepath.Join(m.Path(), "dir", "moved.txt")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(m.Path(), "dir", "moved.txt"))
	if err != nil || string(data) != "over webdav" {
		t.Fatalf("read back %q, %v", data, err)
	}
}
