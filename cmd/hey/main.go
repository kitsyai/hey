// hey — universal app runner for the heypkv/kitsy ecosystem.
// Fetches single-binary apps from GitHub Releases, verifies checksums, runs
// them, and manages long-running UI apps (see docs/app-contract-v0.md).
package main

import (
	"fmt"
	"os"
)

var version = "0.1.0-dev" // overridden at release via -X main.version

const usageText = `hey — fetch, verify and run apps

Usage:
  hey <app>[@version] [args...]      run an app (fetched on demand)
  hey run <app>[@version] [args...]  explicit run form
  hey install <app>[@version]        fetch + verify without running
  hey update [<app>]                 re-resolve latest and fetch if newer
  hey ls                             list installed apps
  hey ps                             list running UI apps and services
  hey stop <app>                     stop a running UI app
  hey svc <up|ls|stop|start|logs|conn|rm>  manage local services
  hey which <app>                    print path of the installed binary
  hey cache clean [<app>]            remove cached binaries
  hey version                        print hey's version

Flags (before the app name):
  --registry <path|https-url>  registry override (env HEY_REGISTRY)
  --no-browser                 don't open the browser after a UI start
  --timeout <dur>              UI startup handshake timeout (default 30s)

Environment:
  HEY_HOME      data directory (default ~/.hey)
  HEY_REGISTRY  registry override
  GH_TOKEN      GitHub token (raises API rate limits)
`

func usage() { fmt.Fprint(os.Stderr, usageText) }

// exitCodeError carries a child process's exit code through to os.Exit.
type exitCodeError int

func (e exitCodeError) Error() string { return fmt.Sprintf("exit code %d", int(e)) }

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	var err error
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Printf("hey %s\n", version)
	case "help", "-h", "--help":
		usage()
	case "install":
		err = cmdInstall(args[1:])
	case "update":
		err = cmdUpdate(args[1:])
	case "ls":
		err = cmdLs(args[1:])
	case "ps":
		err = cmdPs(args[1:])
	case "stop":
		err = cmdStop(args[1:])
	case "which":
		err = cmdWhich(args[1:])
	case "cache":
		err = cmdCache(args[1:])
	case "run":
		err = cmdRun(args[1:])
	case "svc":
		err = cmdSvc(args[1:])
	default:
		err = cmdRun(args) // implicit run: hey <app> [args...]
	}

	if err != nil {
		if code, ok := err.(exitCodeError); ok {
			os.Exit(int(code))
		}
		fmt.Fprintln(os.Stderr, "hey:", err)
		os.Exit(1)
	}
}
