package main

import (
	"fmt"
	"os"
	"time"

	"github.com/kitsyai/hey/internal/home"
	"github.com/kitsyai/hey/internal/namespace"
	"github.com/kitsyai/hey/internal/svc"
)

const upUsage = `hey up — the ergonomic front door for hey services (docs/design/services.md)

Provisions + starts a service (over the same internal/svc machinery 'hey svc
up' already uses) AND makes sure the discovery hub is running, so
127.0.0.1:8525/services and the browser-facing /svc/<name>/* surface come up
with it. 'hey svc up <pack>' still works — 'up' is the general spelling.

Usage:
  hey up <service>[@version] [<service2> ...]   bring up one or more services
  hey up <service> --name <instance>            a second, differently-named instance
  hey up vinci [--model <tier>]                 select a model tier (default: small)
  hey down <service>                            stop a running service

Examples:
  hey up postgres
  hey up vinci postgres
  hey up vinci --model tiny
`

func cmdUp(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(os.Stderr, upUsage)
		if len(args) == 0 {
			return exitCodeError(2)
		}
		return nil
	}
	var refs []string
	var instName, modelTier string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			if i+1 >= len(args) {
				return fmt.Errorf("--name needs a value")
			}
			instName = args[i+1]
			i++
		case "--model":
			if i+1 >= len(args) {
				return fmt.Errorf("--model needs a value")
			}
			modelTier = args[i+1]
			i++
		default:
			refs = append(refs, args[i])
		}
	}
	if len(refs) == 0 {
		return fmt.Errorf("usage: hey up <service>[@version] [<service2> ...] [--name <instance>] [--model <tier>]")
	}
	if instName != "" && len(refs) != 1 {
		return fmt.Errorf("--name only applies when bringing up a single service")
	}

	heyHome, err := home.Dir()
	if err != nil {
		return err
	}
	packs, err := svc.LoadPacks(heyHome)
	if err != nil {
		return err
	}
	reg, err := loadRegistry("")
	if err != nil {
		return err
	}
	// docs/design/services.md §10 open question 5, resolved: one collision
	// check across Reserved commands, app names, and pack names before any
	// service comes up — a pack or app silently shadowing a hub-known name
	// is refused here, not discovered later as a routing bug.
	if err := namespace.CheckCollisions(reg, packs, nil); err != nil {
		return err
	}

	// Validate every ref's pack + instance name up front, before starting
	// anything — a typo in the second of three services shouldn't leave the
	// first one provisioned and the hub started for nothing.
	type resolved struct {
		name, version string
		pack          svc.Pack
	}
	var plan []resolved
	for _, ref := range refs {
		packName, pinned := splitAppRef(ref)
		name := instName
		if name == "" {
			name = packName
		}
		if err := namespace.CheckName(name, packName, reg, packs); err != nil {
			return err
		}
		pack, err := packs.Get(packName)
		if err != nil {
			return err
		}
		version := pinned
		if version == "" {
			version = pack.LatestVersion()
		}
		plan = append(plan, resolved{name: name, version: version, pack: pack})
	}

	if err := ensureHubUp(); err != nil {
		return fmt.Errorf("hey up: could not start the hub: %w", err)
	}

	for _, r := range plan {
		if err := upOne(r.name, r.pack, r.version, modelTier); err != nil {
			return err
		}
	}
	return nil
}

// ensureHubUp starts the hub in the background if it isn't already running —
// docs/design/services.md §4's `hey up <service>` step 1.
func ensureHubUp() error {
	_, _, port, err := hubHandler()
	if err != nil {
		return err
	}
	if EnsureHubRunning(port) {
		return nil
	}
	return startHubDetached()
}

func upOne(name string, pack svc.Pack, version, modelTier string) error {
	svcDir, err := home.SvcDir()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "hey: bringing up %s (%s %s)\n", name, pack.Pack, version)

	var inst *svc.Instance
	if pack.Driver == svc.DriverModelServer {
		inst, err = svc.UpModel(svcDir, name, pack, version, modelTier, nil, nil, modelProgress(pack, modelTier))
	} else {
		inst, err = svc.Up(svcDir, name, pack, version, nil)
	}
	if err != nil {
		return err
	}

	_, _, hubPort, herr := hubHandler()
	if herr == nil {
		fmt.Printf("%s is up: %s %s on 127.0.0.1:%d (pid %d)\n", inst.Name, inst.Pack, inst.Version, inst.Port, inst.PID)
		if pack.Bridge != nil {
			fmt.Printf("  bridge: http://127.0.0.1:%d%s\n", hubPort, pack.Bridge.Path)
		} else if pack.Protocol != "" {
			fmt.Printf("  hub:    http://127.0.0.1:%d/svc/%s\n", hubPort, name)
		}
	}
	if conn := svc.Conn(inst, pack); conn != "" {
		fmt.Printf("  conn: %s\n", conn)
	}
	return nil
}

// modelProgress renders first-run download UX for a model-server pack's
// (potentially multi-GB) model artifact — docs/design/services.md §8's
// "clear progress for the multi-GB model download" P1 requirement. Output is
// throttled to roughly 5/sec so a fast local disk-cache hit doesn't spam.
func modelProgress(pack svc.Pack, tier string) func(done, total int64) {
	label := pack.Pack
	if tier != "" {
		label = fmt.Sprintf("%s (%s)", pack.Pack, tier)
	}
	var last time.Time
	return func(done, total int64) {
		now := time.Now()
		if now.Sub(last) < 200*time.Millisecond && total != 0 && done != total {
			return
		}
		last = now
		if total > 0 {
			pct := float64(done) / float64(total) * 100
			fmt.Fprintf(os.Stderr, "\rhey: downloading %s model: %s / %s (%.1f%%)   ",
				label, humanSize(done), humanSize(total), pct)
		} else {
			fmt.Fprintf(os.Stderr, "\rhey: downloading %s model: %s   ", label, humanSize(done))
		}
		if total != 0 && done == total {
			fmt.Fprintln(os.Stderr)
		}
	}
}

const downUsage = `hey down — stop a service started with 'hey up' (alias of 'hey svc stop')

Usage:
  hey down <instance>
`

func cmdDown(args []string) error {
	if len(args) != 1 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(os.Stderr, downUsage)
		if len(args) != 1 {
			return exitCodeError(2)
		}
		return nil
	}
	return svcStop(args)
}
