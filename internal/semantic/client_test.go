package semantic

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestNativeProcessIsLazyAndTimeoutKillsIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process fixture")
	}
	path := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nwhile read line; do\n case \"$line\" in\n *'\"op\":\"init\"'*) echo '{\"protocol\":2}';;\n *'\"op\":\"stall\"'*) exec sleep 30;;\n *) echo '{}';;\n esac\ndone\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c := NewClient(path, Options{Protocol: ProtocolVersion})
	defer c.Close()
	if c.Running() {
		t.Fatal("constructing client eagerly started runtime")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Call(ctx, Request{Op: "stall"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second || c.Running() {
		t.Fatal("timed out process was not terminated")
	}
	if _, err = c.Call(t.Context(), Request{Op: "list"}); err != nil {
		t.Fatal("process did not restart on demand:", err)
	}
	c.Close()
	if c.Running() {
		t.Fatal("close retained process")
	}
}

func TestManifestRejectsWrongPlatformAndMissingHashes(t *testing.T) {
	for _, m := range []Manifest{{Protocol: ProtocolVersion, Platform: "other/cpu", Version: "v1"}, {Protocol: ProtocolVersion, Platform: Platform(), Version: "v1", Assets: []Asset{{Name: WorkerName(), Kind: "index", Size: 1, SHA256: "bad"}}}, {Protocol: ProtocolVersion, Platform: Platform(), Version: "v1", Assets: []Asset{{Name: "../worker", Kind: "index", Size: 1, SHA256: "bad"}}}} {
		if validateManifest(m, false) == nil {
			t.Fatal("accepted unsafe manifest")
		}
	}
}
