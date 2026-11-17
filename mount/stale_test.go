//go:build linux || darwin

package mount

import (
	"context"
	"net"
	"testing"
)

func TestServerAlive(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if !serverAlive(context.Background(), port) {
		t.Fatal("listening port reported dead")
	}
	ln.Close()
	if serverAlive(context.Background(), port) {
		t.Fatal("closed port reported alive")
	}
}

func TestOptPort(t *testing.T) {
	if p, ok := optPort("vers=3,tcp,port=4242,mountport=4242"); !ok || p != 4242 {
		t.Fatalf("optPort = %d, %v", p, ok)
	}
	if _, ok := optPort("vers=3,tcp"); ok {
		t.Fatal("found a port that is not there")
	}
}
