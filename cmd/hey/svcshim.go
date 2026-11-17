package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kitsyai/hey/internal/svc"
)

// serviceShimMarker identifies shims hey wrote for a service, so hey never
// overwrites or deletes a file it didn't create.
const serviceShimMarker = "hey service shim"

func shimDir() (string, string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return filepath.Dir(self), self, nil
}

func shimPath(dir, command string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(dir, command+".cmd")
	}
	return filepath.Join(dir, command)
}

func shimContent(self, instance, command string) string {
	if runtime.GOOS == "windows" {
		return "@echo off\r\nrem " + serviceShimMarker + " (" + instance + ")\r\n\"" + self + "\" svc exec " + instance + " " + command + " %*\r\n"
	}
	return "#!/bin/sh\n# " + serviceShimMarker + " (" + instance + ")\nexec \"" + self + "\" svc exec " + instance + " " + command + " \"$@\"\n"
}

// ownedByHey reports whether path is absent or a shim hey wrote.
func ownedByHey(path string) (exists, ours bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, true
	}
	return true, strings.Contains(string(data), serviceShimMarker)
}

// installServiceShims puts the pack's commands on PATH next to the hey
// executable (the directory the installer put on PATH), like `hey buddy
// install` does for tools. A file hey didn't write is never replaced.
func installServiceShims(instance string, pack svc.Pack) []string {
	if len(pack.Commands) == 0 {
		return nil
	}
	dir, self, err := shimDir()
	if err != nil {
		return nil
	}
	var added []string
	for _, command := range pack.Commands {
		path := shimPath(dir, command)
		if exists, ours := ownedByHey(path); exists && !ours {
			fmt.Fprintf(os.Stderr, "hey: not adding `%s` to PATH: %s already exists; use `hey svc exec %s %s`\n", command, path, instance, command)
			continue
		}
		if err := os.WriteFile(path, []byte(shimContent(self, instance, command)), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "hey: could not add `%s` to PATH (%v); use `hey svc exec %s %s`\n", command, err, instance, command)
			continue
		}
		added = append(added, command)
	}
	return added
}

// removeServiceShims deletes the shims installServiceShims wrote for
// instance (only files carrying hey's marker).
func removeServiceShims(instance string, pack svc.Pack) {
	dir, _, err := shimDir()
	if err != nil {
		return
	}
	for _, command := range pack.Commands {
		path := shimPath(dir, command)
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), serviceShimMarker+" ("+instance+")") {
			_ = os.Remove(path)
		}
	}
}

// svcExec runs one of a service's declared commands from the instance's
// current binaries, in the foreground with inherited stdio and exit code.
func svcExec(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: hey svc exec <instance> <command> [args...]")
	}
	name, command := args[0], args[1]
	inst, _, err := loadInstance(name)
	if err != nil {
		return err
	}
	packs, err := loadPacks()
	if err != nil {
		return err
	}
	pack, err := packs.Get(inst.Pack)
	if err != nil {
		return err
	}
	allowed := false
	for _, c := range pack.Commands {
		if c == command {
			allowed = true
		}
	}
	if !allowed {
		return fmt.Errorf("%s does not provide a `%s` command (provides: %s)", name, command, strings.Join(pack.Commands, ", "))
	}
	return runPassthrough(filepath.Join(inst.ExeDir(), command+exeExt()), args[2:])
}
