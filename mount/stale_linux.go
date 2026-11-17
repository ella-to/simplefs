package mount

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"
)

// loopbackMounts parses /proc/self/mountinfo for NFSv3 mounts of
// 127.0.0.1:/ that carry an explicit port.
func loopbackMounts(ctx context.Context) ([]staleMount, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseMountinfo(bufio.NewScanner(f)), nil
}

func parseMountinfo(sc *bufio.Scanner) []staleMount {
	var out []staleMount
	for sc.Scan() {
		// 36 35 0:42 / /mnt/x rw,relatime shared:1 - nfs 127.0.0.1:/ rw,vers=3,...,port=40000,...
		pre, post, ok := strings.Cut(sc.Text(), " - ")
		if !ok {
			continue
		}
		fields, tail := strings.Fields(pre), strings.Fields(post)
		if len(fields) < 5 || len(tail) < 3 || tail[0] != "nfs" || tail[1] != "127.0.0.1:/" {
			continue
		}
		if !strings.Contains(","+tail[2]+",", ",vers=3,") {
			continue
		}
		port, ok := optPort(tail[2])
		if !ok {
			continue
		}
		out = append(out, staleMount{path: unescapeMountinfo(fields[4]), port: port})
	}
	return out
}

// unescapeMountinfo decodes the \ooo octal escapes mountinfo uses for
// spaces, tabs, newlines and backslashes in paths.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
