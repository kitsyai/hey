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
  hey <ref>[@version] [args...]      run an app (fetched on demand)
  hey run <ref> [args...]            explicit run form (alias: hey runner run)
  hey runner <run|install|ls|…>      the app-running module (run/install aliased)
  hey install <ref>                  fetch + verify without running
  hey update [<app>|<id>]            update apps and bundles to the latest
  hey ls                             list installed apps and bundles
  hey enable | disable <id>          pause/resume an installed bundle
  hey remove <id>                    delete an installed app or bundle
  hey self update                    update hey itself
  hey ps                             list running UI apps and services
  hey stop <app>                     stop a running UI app
  hey up <service> [<service2> ...]  bring up a hey service (postgres, vinci, ...)
  hey down <service>                 stop a service started with 'hey up'
  hey hub <start|stop|status>        the services discovery hub (127.0.0.1:8525)
  hey svc <up|ls|stop|start|logs|conn|rm>  manage local services (hey up is the general spelling)
  hey registry <start|serve|stop|status|ls|clear>  local package registry (npm)
  hey mobile <devices|push>          nearby-device install (adb)
  hey do <intent> [--param k=v]      run a plan (orchestrate tools for a task)
  hey plan <list|show <intent>>      browse/inspect plans
  hey keeper <auth|ls|rm>            store named credentials (via cnos)
  hey buddy install <ref> [--cred n] fetch+install a bundle (private via keeper)
  hey buddy update [<id>]            update buddy-installed tools to latest
  hey buddy clone <repo> [--cred n]  authenticated git clone [--build "<cmd>"]
  hey open <ref>                     open a link artifact (e.g. TestFlight)
  hey which <app>                    print path of the installed binary
  hey cache clean [<app>]            remove cached binaries and bundles
  hey keygen | sign | verify         publisher signing (ed25519 .heysig)
  hey uninstall                      remove hey and everything it installed
  hey version                        print hey's version

<ref> is an app name (guten), a scoped id (@heypkv/main), or a direct
https manifest URL (https://…/app.json). See docs/deployment-manifest-v0.md.

Flags (before the ref):
  --registry <path|https-url>  registry override (env HEY_REGISTRY)
  --no-browser                 don't open the browser after a UI start
  --timeout <dur>              UI startup handshake timeout (default 30s)
  --channel <name>             deploy channel for @scope/id (default stable)
  --temp                       run a bundle from a throwaway dir, delete on exit
  --location <path>            install a bundle to a caller-chosen directory

Environment:
  HEY_HOME       data directory (default ~/.hey)
  HEY_REGISTRY   registry override
  HEY_HUB_PORT   discovery hub port (default 8525)
  HEY_HUB_CONFIG hub config override (path or https URL; default ~/.hey/hub.json)
  GH_TOKEN       GitHub token (raises API rate limits)
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
	case "up":
		err = cmdUp(args[1:])
	case "down":
		err = cmdDown(args[1:])
	case "hub":
		err = cmdHub(args[1:])
	case "registry":
		err = cmdRegistry(args[1:])
	case "mobile":
		err = cmdMobile(args[1:])
	case "open":
		err = cmdOpen(args[1:])
	case "keygen":
		err = cmdKeygen(args[1:])
	case "sign":
		err = cmdSign(args[1:])
	case "verify":
		err = cmdVerify(args[1:])
	case "enable":
		err = cmdEnable(args[1:])
	case "disable":
		err = cmdDisable(args[1:])
	case "remove", "rm":
		err = cmdRemove(args[1:])
	case "self":
		err = cmdSelf(args[1:])
	case "do":
		err = cmdDo(args[1:])
	case "plan":
		err = cmdPlan(args[1:])
	case "keeper":
		err = cmdKeeper(args[1:])
	case "buddy":
		err = cmdBuddy(args[1:])
	case "runner":
		err = cmdRunner(args[1:])
	case "uninstall":
		err = cmdUninstall(args[1:])
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
