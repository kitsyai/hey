// svcprobe is a synthetic "service" used only by the svc driver tests. Like
// internal/testapp it is never shipped. It optionally writes an init marker
// (to exercise init-once), binds a loopback TCP port and stays up until it is
// signaled, then exits cleanly — enough to drive the archive-exec driver's
// full provision → init → start → ready → stop lifecycle with no network.
package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	args := os.Args[1:]
	port := ""
	initFile := ""
	modelPath := ""
	modelMarker := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 < len(args) {
				port = args[i+1]
				i++
			}
		case "--init":
			// Write a marker file and exit (simulates a one-time init step).
			if i+1 < len(args) {
				initFile = args[i+1]
				i++
			}
		case "--model":
			// model-server driver tests: proves {model} really reached the
			// start command's argv, substituted, not just recorded in svc.json.
			if i+1 < len(args) {
				modelPath = args[i+1]
				i++
			}
		case "--model-marker":
			if i+1 < len(args) {
				modelMarker = args[i+1]
				i++
			}
		}
	}
	if modelMarker != "" {
		if err := os.WriteFile(modelMarker, []byte(modelPath), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "model-marker:", err)
			os.Exit(1)
		}
	}

	if initFile != "" {
		if err := os.WriteFile(initFile, []byte("initialized\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "init:", err)
			os.Exit(1)
		}
		return
	}

	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "svcprobe listening on %s\n", ln.Addr())

	// Exit cleanly on a termination signal (the Unix graceful-stop path).
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		os.Exit(0)
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		conn.Close()
	}
}
