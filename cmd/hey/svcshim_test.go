package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceShimOwnership(t *testing.T) {
	dir := t.TempDir()
	path := shimPath(dir, "dsc")
	if exists, ours := ownedByHey(path); exists || !ours {
		t.Fatal("an absent file can be written")
	}
	os.WriteFile(path, []byte(shimContent("/opt/hey/hey", "dsc", "dsc")), 0o755)
	if exists, ours := ownedByHey(path); !exists || !ours {
		t.Fatal("hey's own shim must be recognized so it can be refreshed and removed")
	}
	other := filepath.Join(dir, "psql")
	os.WriteFile(other, []byte("#!/bin/sh\necho mine\n"), 0o755)
	if _, ours := ownedByHey(other); ours {
		t.Fatal("a file hey didn't write must never be replaced")
	}
	content := shimContent("/opt/hey/hey", "dsc", "dsc")
	if !strings.Contains(content, `svc exec dsc dsc`) {
		t.Fatalf("shim must delegate to hey svc exec: %q", content)
	}
}
