package npm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/kitsyai/hey/internal/pkgreg"
)

// localVersion is one locally-published version, as recorded in a package's
// metadata.json.
type localVersion struct {
	Doc       json.RawMessage `json:"doc"`       // the full version manifest exactly as published (dist.tarball rewritten at serve time)
	Tarball   string          `json:"tarball"`   // tarball file name, e.g. "theme-0.1.0.tgz"
	Tag       string          `json:"tag"`       // dist-tag this version was published under ("latest" if unspecified)
	Published time.Time       `json:"published"`
}

// localPackage is the on-disk metadata.json for one package: every locally
// published version, keyed by version string.
type localPackage struct {
	Name     string                  `json:"name"`
	Versions map[string]localVersion `json:"versions"`
}

// pkgDir returns <store>/<pkg>, e.g. <store>/@heypkv/theme or <store>/lodash.
// Store layout is namespaced per the architecture directive: the npm Format
// only ever sees the npm subtree (<hey-registry-store>/npm), handed to it
// already-resolved via Core.FormatDir; pkgDir just joins the package name
// onto it, so on-disk layout is <store>/npm/<pkg>/{metadata.json,tarballs/}.
func pkgDir(npmStoreRoot, pkg string) string {
	return filepath.Join(npmStoreRoot, filepath.FromSlash(pkg))
}

func metadataPath(npmStoreRoot, pkg string) string {
	return filepath.Join(pkgDir(npmStoreRoot, pkg), "metadata.json")
}

func tarballsDir(npmStoreRoot, pkg string) string {
	return filepath.Join(pkgDir(npmStoreRoot, pkg), "tarballs")
}

// loadPackage reads a package's local metadata.json. A package with no
// local publishes yet returns an empty, valid localPackage.
func loadPackage(npmStoreRoot, pkg string) (*localPackage, error) {
	b, err := os.ReadFile(metadataPath(npmStoreRoot, pkg))
	if os.IsNotExist(err) {
		return &localPackage{Name: pkg, Versions: map[string]localVersion{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var lp localPackage
	if err := json.Unmarshal(b, &lp); err != nil {
		return nil, fmt.Errorf("parse %s: %w", metadataPath(npmStoreRoot, pkg), err)
	}
	if lp.Versions == nil {
		lp.Versions = map[string]localVersion{}
	}
	return &lp, nil
}

// savePackage writes a package's metadata.json atomically.
func savePackage(npmStoreRoot string, lp *localPackage) error {
	dir := pkgDir(npmStoreRoot, lp.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(lp, "", "  ")
	if err != nil {
		return err
	}
	path := metadataPath(npmStoreRoot, lp.Name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// tarballPath returns the on-disk path a local tarball would live at,
// regardless of whether it exists yet.
func tarballPath(npmStoreRoot, pkg, filename string) string {
	return filepath.Join(tarballsDir(npmStoreRoot, pkg), filename)
}

// localTarball returns the path to filename under pkg's local tarballs if
// it exists, and whether it was found.
func localTarball(npmStoreRoot, pkg, filename string) (string, bool) {
	p := tarballPath(npmStoreRoot, pkg, filename)
	if info, err := os.Stat(p); err == nil && !info.IsDir() {
		return p, true
	}
	return "", false
}

// listPackages enumerates every package with local publishes under
// npmStoreRoot (scoped packages are one level deeper: @scope/name).
func listPackages(npmStoreRoot string) ([]string, error) {
	var pkgs []string
	scopes, err := os.ReadDir(npmStoreRoot)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range scopes {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(npmStoreRoot, e.Name(), "metadata.json")); err == nil {
			pkgs = append(pkgs, e.Name())
			continue
		}
		if !filepathHasScopePrefix(e.Name()) {
			continue
		}
		subs, err := os.ReadDir(filepath.Join(npmStoreRoot, e.Name()))
		if err != nil {
			continue
		}
		for _, sub := range subs {
			if !sub.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(npmStoreRoot, e.Name(), sub.Name(), "metadata.json")); err == nil {
				pkgs = append(pkgs, e.Name()+"/"+sub.Name())
			}
		}
	}
	sort.Strings(pkgs)
	return pkgs, nil
}

func filepathHasScopePrefix(name string) bool {
	return len(name) > 0 && name[0] == '@'
}

// List implements pkgreg.Format for npm — every locally-published
// package@version, for `hey registry ls`/`status`.
func (f *Format) List(storeRoot string) ([]pkgreg.PublishedEntry, error) {
	root := filepath.Join(storeRoot, formatName)
	pkgs, err := listPackages(root)
	if err != nil {
		return nil, err
	}
	var out []pkgreg.PublishedEntry
	for _, pkg := range pkgs {
		lp, err := loadPackage(root, pkg)
		if err != nil {
			return nil, err
		}
		versions := make([]string, 0, len(lp.Versions))
		for v := range lp.Versions {
			versions = append(versions, v)
		}
		sort.Strings(versions)
		for _, v := range versions {
			out = append(out, pkgreg.PublishedEntry{Format: formatName, Package: pkg, Version: v})
		}
	}
	return out, nil
}

// Clear implements pkgreg.Format for npm — drop local versions matching
// pkg/version (either may be empty; empty pkg clears the whole npm store).
func (f *Format) Clear(storeRoot, pkg, version string) (int, error) {
	root := filepath.Join(storeRoot, formatName)
	if pkg == "" {
		pkgs, err := listPackages(root)
		if err != nil {
			return 0, err
		}
		total := 0
		for _, p := range pkgs {
			n, err := clearPackage(root, p, "")
			if err != nil {
				return total, err
			}
			total += n
		}
		return total, nil
	}
	return clearPackage(root, pkg, version)
}

func clearPackage(npmStoreRoot, pkg, version string) (int, error) {
	if version == "" {
		lp, err := loadPackage(npmStoreRoot, pkg)
		if err != nil {
			return 0, err
		}
		n := len(lp.Versions)
		if n == 0 {
			return 0, nil
		}
		if err := os.RemoveAll(pkgDir(npmStoreRoot, pkg)); err != nil {
			return 0, err
		}
		return n, nil
	}

	lp, err := loadPackage(npmStoreRoot, pkg)
	if err != nil {
		return 0, err
	}
	lv, ok := lp.Versions[version]
	if !ok {
		return 0, nil
	}
	delete(lp.Versions, version)
	_ = os.Remove(tarballPath(npmStoreRoot, pkg, lv.Tarball))
	if len(lp.Versions) == 0 {
		if err := os.RemoveAll(pkgDir(npmStoreRoot, pkg)); err != nil {
			return 0, err
		}
		return 1, nil
	}
	if err := savePackage(npmStoreRoot, lp); err != nil {
		return 0, err
	}
	return 1, nil
}
