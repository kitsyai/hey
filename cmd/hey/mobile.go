package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kitsyai/hey/internal/deploy"
	"github.com/kitsyai/hey/internal/home"
)

const mobileUsage = `hey mobile — install builds to nearby devices (until a hey mobile client exists)

Usage:
  hey mobile devices                              list reachable devices (adb)
  hey mobile push <ref> [--device <id>] [--channel <c>]  install an apk on a device

Requires Android Platform Tools (adb) on PATH: https://developer.android.com/tools/releases/platform-tools
`

func cmdMobile(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, mobileUsage)
		return exitCodeError(2)
	}
	switch args[0] {
	case "devices":
		return mobileDevices(args[1:])
	case "push":
		return mobilePush(args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(os.Stderr, mobileUsage)
		return nil
	default:
		return fmt.Errorf("unknown mobile subcommand %q\n%s", args[0], mobileUsage)
	}
}

// findADB locates the adb executable, returning a friendly, non-fatal message
// when it is missing so `hey mobile` guides the user instead of crashing.
func findADB() (string, error) {
	path, err := exec.LookPath("adb")
	if err != nil {
		return "", fmt.Errorf("adb (Android Platform Tools) is not on PATH.\n" +
			"  Install it from https://developer.android.com/tools/releases/platform-tools\n" +
			"  and ensure `adb` is on PATH, then re-run `hey mobile`.")
	}
	return path, nil
}

// device is one entry from `adb devices`.
type device struct {
	Serial string
	State  string // device | offline | unauthorized | ...
}

// parseDevices parses `adb devices` output (both USB serials and same-network
// host:port entries appear here).
func parseDevices(out string) []device {
	var devs []device
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of devices") || strings.HasPrefix(line, "*") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		devs = append(devs, device{Serial: fields[0], State: fields[1]})
	}
	return devs
}

func mobileDevices(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: hey mobile devices")
	}
	adb, err := findADB()
	if err != nil {
		// Non-fatal: print guidance and succeed so the binary never hard-fails
		// just because adb is absent.
		fmt.Fprintln(os.Stderr, "hey:", err)
		return nil
	}
	out, err := exec.Command(adb, "devices").CombinedOutput()
	if err != nil {
		return fmt.Errorf("adb devices: %v\n%s", err, out)
	}
	devs := parseDevices(string(out))
	if len(devs) == 0 {
		fmt.Println("no devices — connect one over USB (with debugging enabled) or `adb connect <ip>`")
		return nil
	}
	fmt.Printf("%-24s %s\n", "DEVICE", "STATE")
	for _, d := range devs {
		fmt.Printf("%-24s %s\n", d.Serial, d.State)
	}
	return nil
}

func mobilePush(args []string) error {
	var refArg, deviceID, channel string
	var allowUntrusted bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--device":
			if i+1 >= len(args) {
				return fmt.Errorf("--device needs a value")
			}
			deviceID = args[i+1]
			i++
		case "--channel":
			if i+1 >= len(args) {
				return fmt.Errorf("--channel needs a value")
			}
			channel = args[i+1]
			i++
		case "--allow-untrusted":
			allowUntrusted = true
		default:
			if refArg != "" {
				return fmt.Errorf("usage: hey mobile push <ref> [--device <id>] [--channel <c>] [--allow-untrusted]")
			}
			refArg = args[i]
		}
	}
	if refArg == "" {
		return fmt.Errorf("usage: hey mobile push <ref> [--device <id>] [--channel <c>] [--allow-untrusted]")
	}

	adb, err := findADB()
	if err != nil {
		return err // push cannot proceed without adb — this one IS a real error
	}

	ref, err := deploy.ParseRef(refArg)
	if err != nil {
		return err
	}
	if ref.Kind == deploy.RefAppName {
		return fmt.Errorf("%q is a github-release app; `hey mobile push` needs a deploy ref (@scope/id or an https manifest URL)", refArg)
	}
	m, err := resolveManifest(ref, deployOpts{channel: channel, allowUntrusted: allowUntrusted})
	if err != nil {
		return err
	}
	art, err := m.SelectPackage("android")
	if err != nil {
		return err
	}

	// Resolve the target device: an explicit --device wins; otherwise require
	// exactly one online device so a push is never sent to a surprise target.
	if deviceID == "" {
		deviceID, err = soleDevice(adb)
		if err != nil {
			return err
		}
	}

	// Download + verify the apk into a throwaway dir.
	tmpDir, err := home.TempInstallDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	apk, err := downloadVerify(art, tmpDir)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "hey: installing %s %s on %s\n", m.ID, m.Version, deviceID)
	cmd := exec.Command(adb, "-s", deviceID, "install", apk)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("adb install: %w", err)
	}
	fmt.Printf("%s %s installed on %s\n", m.ID, m.Version, deviceID)
	return nil
}

// soleDevice returns the single online device, or an error listing the choices
// when there are zero or several (the user must then pass --device).
func soleDevice(adb string) (string, error) {
	out, err := exec.Command(adb, "devices").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("adb devices: %v\n%s", err, out)
	}
	var online []string
	for _, d := range parseDevices(string(out)) {
		if d.State == "device" {
			online = append(online, d.Serial)
		}
	}
	switch len(online) {
	case 1:
		return online[0], nil
	case 0:
		return "", fmt.Errorf("no online devices — connect one (USB debugging on) or `adb connect <ip>`")
	default:
		return "", fmt.Errorf("multiple devices attached (%s) — choose one with --device", strings.Join(online, ", "))
	}
}

// cmdOpen resolves a manifest and opens its link artifact (a store page or
// TestFlight URL). iOS prerelease uses this instead of a device push.
func cmdOpen(args []string) error {
	var refArg, channel string
	var allowUntrusted bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--channel":
			if i+1 >= len(args) {
				return fmt.Errorf("--channel needs a value")
			}
			channel = args[i+1]
			i++
		case "--allow-untrusted":
			allowUntrusted = true
		default:
			if refArg != "" {
				return fmt.Errorf("usage: hey open <ref> [--channel <c>] [--allow-untrusted]")
			}
			refArg = args[i]
		}
	}
	if refArg == "" {
		return fmt.Errorf("usage: hey open <ref> [--channel <c>] [--allow-untrusted]")
	}
	ref, err := deploy.ParseRef(refArg)
	if err != nil {
		return err
	}
	if ref.Kind == deploy.RefAppName {
		return fmt.Errorf("%q is a github-release app; `hey open` needs a deploy ref (@scope/id or an https manifest URL)", refArg)
	}
	m, err := resolveManifest(ref, deployOpts{channel: channel, allowUntrusted: allowUntrusted, timeout: 30 * time.Second})
	if err != nil {
		return err
	}
	link, err := m.SelectLink("")
	if err != nil {
		return err
	}
	fmt.Printf("%s: opening %s\n", m.ID, link.URL)
	openBrowser(link.URL, false)
	return nil
}
