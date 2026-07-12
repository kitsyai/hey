package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/heypkv/hey/internal/home"
	"github.com/heypkv/hey/internal/svc"
)

const svcUsage = `hey svc — provision and manage local services

Usage:
  hey svc up <pack>[@version] [--name <instance>]   provision + start (idempotent)
  hey svc ls                                        list instances
  hey svc stop <instance>                           stop a running instance
  hey svc start <instance>                          start a stopped instance
  hey svc logs <instance> [--tail N]                show the service log
  hey svc conn <instance>                           print the connection string
  hey svc rm <instance> [--purge-data]              remove an instance
`

func cmdSvc(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, svcUsage)
		return exitCodeError(2)
	}
	switch args[0] {
	case "up":
		return svcUp(args[1:])
	case "ls":
		return svcLs(args[1:])
	case "stop":
		return svcStop(args[1:])
	case "start":
		return svcStart(args[1:])
	case "logs":
		return svcLogs(args[1:])
	case "conn":
		return svcConn(args[1:])
	case "rm":
		return svcRm(args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(os.Stderr, svcUsage)
		return nil
	default:
		return fmt.Errorf("unknown svc subcommand %q\n%s", args[0], svcUsage)
	}
}

func loadPacks() (*svc.PackSet, error) {
	heyHome, err := home.Dir()
	if err != nil {
		return nil, err
	}
	return svc.LoadPacks(heyHome)
}

// loadInstance resolves an instance by name from the svc dir.
func loadInstance(name string) (*svc.Instance, string, error) {
	svcDir, err := home.SvcDir()
	if err != nil {
		return nil, "", err
	}
	inst, err := svc.LoadInstance(filepath.Join(svcDir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("no instance %q — see `hey svc ls`", name)
		}
		return nil, "", err
	}
	return inst, svcDir, nil
}

func svcUp(args []string) error {
	var ref, name string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			if i+1 >= len(args) {
				return fmt.Errorf("--name needs a value")
			}
			name = args[i+1]
			i++
		default:
			if ref != "" {
				return fmt.Errorf("usage: hey svc up <pack>[@version] [--name <instance>]")
			}
			ref = args[i]
		}
	}
	if ref == "" {
		return fmt.Errorf("usage: hey svc up <pack>[@version] [--name <instance>]")
	}
	packName, pinned := splitAppRef(ref)
	if name == "" {
		name = packName
	}

	packs, err := loadPacks()
	if err != nil {
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

	svcDir, err := home.SvcDir()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "hey: bringing up %s (%s %s)\n", name, pack.Pack, version)
	inst, err := svc.Up(svcDir, name, pack, version, nil)
	if err != nil {
		return err
	}
	fmt.Printf("%s is up: %s %s on 127.0.0.1:%d (pid %d)\n",
		inst.Name, inst.Pack, inst.Version, inst.Port, inst.PID)
	if conn := svc.Conn(inst, pack); conn != "" {
		fmt.Printf("  conn: %s\n", conn)
	}
	return nil
}

func svcLs(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: hey svc ls")
	}
	svcDir, err := home.SvcDir()
	if err != nil {
		return err
	}
	insts, err := svc.ListInstances(svcDir)
	if err != nil {
		return err
	}
	if len(insts) == 0 {
		fmt.Println("no services — try `hey svc up postgres`")
		return nil
	}
	fmt.Printf("%-14s %-10s %-9s %-6s %-9s %s\n", "INSTANCE", "PACK", "VERSION", "PORT", "STATE", "DATA")
	for _, in := range insts {
		state := svc.StateStopped
		if svc.Alive(in) {
			state = svc.StateRunning
		}
		fmt.Printf("%-14s %-10s %-9s %-6d %-9s %s\n",
			in.Name, in.Pack, in.Version, in.Port, state, humanSize(svc.DirSize(in.DataDir())))
	}
	return nil
}

func svcStop(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: hey svc stop <instance>")
	}
	inst, _, err := loadInstance(args[0])
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
	if err := svc.Stop(inst, pack); err != nil {
		return err
	}
	fmt.Printf("stopped %s\n", inst.Name)
	return nil
}

func svcStart(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: hey svc start <instance>")
	}
	inst, _, err := loadInstance(args[0])
	if err != nil {
		return err
	}
	if svc.Alive(inst) {
		fmt.Printf("%s is already running (pid %d)\n", inst.Name, inst.PID)
		return nil
	}
	packs, err := loadPacks()
	if err != nil {
		return err
	}
	pack, err := packs.Get(inst.Pack)
	if err != nil {
		return err
	}
	if err := svc.Start(inst, pack); err != nil {
		return err
	}
	if err := svc.WaitReady(inst, pack); err != nil {
		return err
	}
	fmt.Printf("%s is up on 127.0.0.1:%d (pid %d)\n", inst.Name, inst.Port, inst.PID)
	return nil
}

func svcLogs(args []string) error {
	tail := 0
	var name string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--tail":
			if i+1 >= len(args) {
				return fmt.Errorf("--tail needs a value")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return fmt.Errorf("bad --tail: %w", err)
			}
			tail = n
			i++
		default:
			name = args[i]
		}
	}
	if name == "" {
		return fmt.Errorf("usage: hey svc logs <instance> [--tail N]")
	}
	inst, _, err := loadInstance(name)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(inst.LogPath())
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("(no log yet)")
			return nil
		}
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if tail > 0 && len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return nil
}

func svcConn(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: hey svc conn <instance>")
	}
	inst, _, err := loadInstance(args[0])
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
	conn := svc.Conn(inst, pack)
	if conn == "" {
		return fmt.Errorf("%s has no connection string", inst.Name)
	}
	fmt.Println(conn)
	return nil
}

func svcRm(args []string) error {
	purge := false
	var name string
	for _, a := range args {
		switch a {
		case "--purge-data":
			purge = true
		default:
			name = a
		}
	}
	if name == "" {
		return fmt.Errorf("usage: hey svc rm <instance> [--purge-data]")
	}
	inst, svcDir, err := loadInstance(name)
	if err != nil {
		return err
	}
	packs, err := loadPacks()
	if err != nil {
		return err
	}
	// Stop it first if we still have its pack (best effort otherwise).
	if svc.Alive(inst) {
		if pack, perr := packs.Get(inst.Pack); perr == nil {
			_ = svc.Stop(inst, pack)
		}
	}

	dir := filepath.Join(svcDir, name)
	if !purge {
		// Refuse to delete the user's data without an explicit opt-in.
		size := svc.DirSize(inst.DataDir())
		return fmt.Errorf("refusing to delete %s: it holds %s of data in %s\n"+
			"  re-run with --purge-data to remove the data permanently",
			name, humanSize(size), inst.DataDir())
	}
	// --purge-data still requires an interactive confirmation.
	fmt.Printf("This permanently deletes %s AND its data (%s). Type the instance name to confirm: ",
		name, humanSize(svc.DirSize(inst.DataDir())))
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	if strings.TrimSpace(line) != name {
		return fmt.Errorf("confirmation did not match; nothing deleted")
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	fmt.Printf("removed %s and its data\n", name)
	return nil
}

// svcSummary prints the services section for `hey ps`.
func svcSummary() {
	svcDir, err := home.SvcDir()
	if err != nil {
		return
	}
	insts, err := svc.ListInstances(svcDir)
	if err != nil || len(insts) == 0 {
		return
	}
	fmt.Println()
	fmt.Printf("%-14s %-10s %-6s %-9s %s\n", "SERVICE", "PACK", "PORT", "STATE", "UPTIME")
	for _, in := range insts {
		state, uptime := svc.StateStopped, "-"
		if svc.Alive(in) {
			state = svc.StateRunning
			if !in.Started.IsZero() {
				uptime = time.Since(in.Started).Round(time.Second).String()
			}
		}
		fmt.Printf("%-14s %-10s %-6d %-9s %s\n", in.Name, in.Pack, in.Port, state, uptime)
	}
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGT"[exp])
}
