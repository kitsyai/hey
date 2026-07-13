package main

import (
	"fmt"
	"time"

	"github.com/kitsyai/hey/internal/contract"
	"github.com/kitsyai/hey/internal/home"
	"github.com/kitsyai/hey/internal/proc"
	"github.com/kitsyai/hey/internal/state"
)

func cmdPs(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: hey ps")
	}
	stateDir, err := home.StateDir()
	if err != nil {
		return err
	}
	// Liveness = process alive AND /healthz answering — the health check
	// sidesteps PID-reuse false positives.
	procs, err := state.Prune(stateDir, func(p state.Proc) bool {
		return proc.Alive(p.PID) && contract.Healthy(p.URL)
	})
	if err != nil {
		return err
	}
	if len(procs) == 0 {
		fmt.Println("no UI apps running")
	} else {
		fmt.Printf("%-12s %-8s %-24s %s\n", "APP", "PID", "URL", "UPTIME")
		for _, p := range procs {
			fmt.Printf("%-12s %-8d %-24s %s\n",
				p.App, p.PID, p.URL, time.Since(p.Started).Round(time.Second))
		}
	}
	svcSummary()
	return nil
}

func cmdStop(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: hey stop <app>")
	}
	name := args[0]
	stateDir, err := home.StateDir()
	if err != nil {
		return err
	}
	entry, ok, err := state.Get(stateDir, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s is not running (per hey's records)", name)
	}

	// Graceful first: the contract's shutdown endpoint, then a short wait.
	if err := contract.Shutdown(entry.URL); err == nil {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if !proc.Alive(entry.PID) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if proc.Alive(entry.PID) {
		if err := proc.KillTree(entry.PID); err != nil {
			return fmt.Errorf("stop %s: %w", name, err)
		}
	}
	if err := state.Remove(stateDir, name); err != nil {
		return err
	}
	fmt.Printf("stopped %s (pid %d)\n", name, entry.PID)
	return nil
}
