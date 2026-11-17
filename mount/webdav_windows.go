package mount

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// attachWebDAV maps url to a drive letter with the built-in WebClient
// service (`net use`), available on every Windows edition including Home.
// WebClient limits files to 50 MB by default (registry:
// HKLM\SYSTEM\CurrentControlSet\Services\WebClient\Parameters\FileSizeLimitInBytes).
func attachWebDAV(ctx context.Context, url string, cfg attachConfig) (string, func(), error) {
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
	out, err := exec.CommandContext(ctx, "net", "use", letter, url, "/persistent:no").CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "67") || strings.Contains(strings.ToLower(msg), "network name cannot be found") {
			return "", nil, fmt.Errorf("%w: the WebClient service is not running "+
				"(start it with: sc start WebClient, or Services > WebClient > Automatic): %s", ErrNoClient, msg)
		}
		return "", nil, fmt.Errorf("mount: net use %s %s: %s: %w", letter, url, msg, err)
	}
	return letter + `\`, func() {}, nil
}

func detachWebDAV(target string) error {
	letter := strings.TrimSuffix(target, `\`)
	out, err := exec.Command("net", "use", letter, "/delete", "/y").CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount: net use %s /delete: %s: %w", letter, strings.TrimSpace(string(out)), err)
	}
	return nil
}
