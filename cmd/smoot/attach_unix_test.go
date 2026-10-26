//go:build unix

package main

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestReadCappedRefusesFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCapped(path, 10); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v", err)
	}
}
