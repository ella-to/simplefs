package mount

import (
	"bufio"
	"reflect"
	"strings"
	"testing"
)

func TestParseMountinfo(t *testing.T) {
	in := `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
40 22 0:50 / /mnt/my\040dir rw,relatime shared:2 - nfs 127.0.0.1:/ rw,vers=3,rsize=1048576,proto=tcp,port=40001,mountport=40001,addr=127.0.0.1
41 22 0:51 / /mnt/v4 rw - nfs4 127.0.0.1:/ rw,vers=4.2,port=2049
42 22 0:52 / /mnt/remote rw - nfs 10.0.0.5:/export rw,vers=3,port=2049
`
	got := parseMountinfo(bufio.NewScanner(strings.NewReader(in)))
	want := []staleMount{{path: "/mnt/my dir", port: 40001}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
