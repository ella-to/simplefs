package mount

import (
	"bufio"
	"reflect"
	"strings"
	"testing"
)

func TestParseNfsstat(t *testing.T) {
	in := `/Users/me/OrbStack from OrbStack:/OrbStack
  -- Original mount options:
     NFS parameters: vers=4.0,port=60948,soft
/Users/me/Library/Caches/simplefs/x-1 from 127.0.0.1:/
  -- Original mount options:
     General mount flags: 0x100000 nobrowse
     NFS parameters: vers=3,tcp,port=61179,mountport=61179,soft
  -- Current mount parameters:
     NFS parameters: vers=3,tcp,port=61179,mountport=61179,soft
/Volumes/v4 from 127.0.0.1:/
  -- Original mount options:
     NFS parameters: vers=4,tcp,port=2049
`
	got := parseNfsstat(bufio.NewScanner(strings.NewReader(in)))
	want := []staleMount{{path: "/Users/me/Library/Caches/simplefs/x-1", port: 61179}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
