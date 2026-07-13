package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/kitsyai/hey/internal/deploy"
	"github.com/kitsyai/hey/internal/home"
)

func cmdInstall(args []string) error {
	registryOverride, channel, location, rest, err := takeInstallFlags(args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: hey install <ref> [--channel <c>] [--location <path>]")
	}

	// Deploy refs (@scope/id or a direct https manifest URL) install a
	// hey.deploy.v1 bundle; bare names stay on the legacy github-release path.
	if ref, rerr := deploy.ParseRef(rest[0]); rerr == nil && ref.Kind != deploy.RefAppName {
		return installDeployRef(ref, deployOpts{
			registryOverride: registryOverride,
			channel:          channel,
			location:         location,
			timeout:          30 * time.Second,
		})
	} else if rerr != nil {
		return rerr
	}

	name, pinned := splitAppRef(rest[0])
	reg, err := loadRegistry(registryOverride)
	if err != nil {
		return err
	}
	app, err := lookupApp(reg, name)
	if err != nil {
		return err
	}
	version, err := resolveVersion(name, app, pinned, false)
	if err != nil {
		return err
	}
	binPath, err := ensureInstalled(name, app, version)
	if err != nil {
		return err
	}
	fmt.Printf("%s %s -> %s\n", name, version, binPath)
	return nil
}

func cmdUpdate(args []string) error {
	registryOverride, rest, err := takeRegistryFlag(args)
	if err != nil {
		return err
	}
	reg, err := loadRegistry(registryOverride)
	if err != nil {
		return err
	}

	var names []string
	if len(rest) == 1 {
		names = []string{rest[0]}
	} else if len(rest) == 0 {
		names = installedApps()
		if len(names) == 0 {
			fmt.Println("nothing installed yet — try `hey install <app>`")
			return nil
		}
	} else {
		return fmt.Errorf("usage: hey update [<app>]")
	}

	for _, name := range names {
		app, err := lookupApp(reg, name)
		if err != nil {
			return err
		}
		version, err := resolveVersion(name, app, "", true) // bypass resolve cache
		if err != nil {
			return err
		}
		if cur, ok := currentVersion(name); ok && cur == version {
			fmt.Printf("%s %s is already the latest\n", name, version)
			continue
		}
		if _, err := ensureInstalled(name, app, version); err != nil {
			return err
		}
		fmt.Printf("%s updated to %s\n", name, version)
	}
	return nil
}

func cmdLs(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: hey ls")
	}
	names := installedApps()
	deployed := deployedApps()
	if len(names) == 0 && len(deployed) == 0 {
		fmt.Println("nothing installed yet — try `hey install <app>`")
		return nil
	}
	binDir, err := home.BinDir()
	if err != nil {
		return err
	}
	for _, name := range names {
		cur, _ := currentVersion(name)
		versions := installedVersions(name)
		fmt.Printf("%-12s current %-10s versions [%s]  %s\n",
			name, cur, joinStrings(versions, ", "), filepath.Join(binDir, name))
	}
	if len(deployed) > 0 {
		appsDir, err := home.AppsDir()
		if err != nil {
			return err
		}
		fmt.Println("\ndeployed bundles (hey.deploy.v1):")
		for _, id := range deployed {
			versions := deployedVersions(id)
			fmt.Printf("%-12s versions [%s]  %s\n",
				id, joinStrings(versions, ", "), filepath.Join(appsDir, id))
		}
	}
	return nil
}

func cmdWhich(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: hey which <app>")
	}
	name, pinned := splitAppRef(args[0])
	version := pinned
	if version == "" {
		version, _ = currentVersion(name) // may be "" for a deploy-only id
	}
	if version != "" {
		appDir, err := home.AppDir(name)
		if err != nil {
			return err
		}
		// Try both plain and .exe names so `which` works for any cached platform.
		for _, candidate := range []string{name + ".exe", name} {
			p := filepath.Join(appDir, version, candidate)
			if _, err := os.Stat(p); err == nil {
				fmt.Println(p)
				return nil
			}
		}
	}
	// Fall back to a deployed bundle: print its install directory.
	if dir, ok := deployedDir(name, pinned); ok {
		fmt.Println(dir)
		return nil
	}
	return fmt.Errorf("%s is not installed", name)
}

func cmdCache(args []string) error {
	if len(args) == 0 || args[0] != "clean" {
		return fmt.Errorf("usage: hey cache clean [<app>]")
	}
	binDir, err := home.BinDir()
	if err != nil {
		return err
	}
	appsDir, err := home.AppsDir()
	if err != nil {
		return err
	}
	switch len(args) {
	case 1:
		names := installedApps()
		for _, name := range names {
			if err := os.RemoveAll(filepath.Join(binDir, name)); err != nil {
				return err
			}
		}
		deployed := deployedApps()
		for _, id := range deployed {
			if err := os.RemoveAll(filepath.Join(appsDir, id)); err != nil {
				return err
			}
		}
		fmt.Printf("removed %d cached app(s) and %d deployed bundle(s)\n", len(names), len(deployed))
	case 2:
		name, _ := splitAppRef(args[1])
		removed := false
		for _, target := range []string{filepath.Join(binDir, name), filepath.Join(appsDir, name)} {
			if _, err := os.Stat(target); err == nil {
				if err := os.RemoveAll(target); err != nil {
					return err
				}
				removed = true
			}
		}
		if !removed {
			return fmt.Errorf("%s is not installed", name)
		}
		fmt.Printf("removed cached %s\n", name)
	default:
		return fmt.Errorf("usage: hey cache clean [<app>]")
	}
	return nil
}

// --- helpers ---

// takeInstallFlags parses the flags `hey install` understands (registry,
// channel, location) and returns the remaining positional args.
func takeInstallFlags(args []string) (override, channel, location string, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--registry", "--channel", "--location":
			if i+1 >= len(args) {
				return "", "", "", nil, fmt.Errorf("%s needs a value", args[i])
			}
			switch args[i] {
			case "--registry":
				override = args[i+1]
			case "--channel":
				channel = args[i+1]
			case "--location":
				location = args[i+1]
			}
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	return override, channel, location, rest, nil
}

func takeRegistryFlag(args []string) (override string, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--registry" {
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("--registry needs a value")
			}
			override = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	return override, rest, nil
}

func installedApps() []string {
	binDir, err := home.BinDir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

func installedVersions(name string) []string {
	appDir, err := home.AppDir(name)
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(appDir)
	if err != nil {
		return nil
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() && e.Name()[0] != '.' {
			versions = append(versions, e.Name())
		}
	}
	sort.Strings(versions)
	return versions
}

// deployedApps lists the ids of installed hey.deploy.v1 bundles (dirs under
// ~/.hey/apps), sorted.
func deployedApps() []string {
	appsDir, err := home.AppsDir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(appsDir)
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids
}

// deployedVersions lists the installed versions of a deployed bundle, sorted.
func deployedVersions(id string) []string {
	appsDir, err := home.AppsDir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(appsDir, id))
	if err != nil {
		return nil
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() && e.Name()[0] != '.' {
			versions = append(versions, e.Name())
		}
	}
	sort.Strings(versions)
	return versions
}

// deployedDir returns the install dir of a deployed bundle. With no version it
// picks the highest (last sorted) installed version.
func deployedDir(id, version string) (string, bool) {
	appsDir, err := home.AppsDir()
	if err != nil {
		return "", false
	}
	if version == "" {
		versions := deployedVersions(id)
		if len(versions) == 0 {
			return "", false
		}
		version = versions[len(versions)-1]
	}
	dir := filepath.Join(appsDir, id, version)
	if _, err := os.Stat(dir); err != nil {
		return "", false
	}
	return dir, true
}

func joinStrings(s []string, sep string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += sep
		}
		out += v
	}
	return out
}
