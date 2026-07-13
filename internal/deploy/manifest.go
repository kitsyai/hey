// Package deploy implements hey's side of the deployment manifest v0
// (docs/deployment-manifest-v0.md): the hey.deploy.v1 document a producer
// publishes to describe how hey installs, launches, verifies and cleans up an
// app. hey knows an app ONLY through this manifest — there is no product logic
// here and no hey logic in any product. The manifest is the entire interface.
package deploy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"
)

// SchemaVersion is the hey.deploy.v1 manifest version this build understands.
const SchemaVersion = 1

// maxManifestBytes caps a manifest download (manifests are tiny JSON).
const maxManifestBytes = 1 << 20 // 1 MB

// Manifest is a parsed hey.deploy.v1 document.
type Manifest struct {
	HeyDeploy int        `json:"hey_deploy"`
	ID        string     `json:"id"`
	Name      string     `json:"name,omitempty"`
	Version   string     `json:"version"`
	Channel   string     `json:"channel,omitempty"`
	Artifacts []Artifact `json:"artifacts"`
}

// Artifact is one platform build named by a manifest.
type Artifact struct {
	Platform  string `json:"platform"`            // macos | windows | linux | android | ios
	Arch      string `json:"arch,omitempty"`      // arm64 | x64 | universal (omit for mobile/link)
	Kind      string `json:"kind"`                // archive | appimage | binary | installer | package | link
	Format    string `json:"format,omitempty"`    // archive only: zip | tar.gz
	URL       string `json:"url"`
	SHA256    string `json:"sha256,omitempty"`    // REQUIRED for every downloadable artifact
	Size      int64  `json:"size,omitempty"`
	Launch    Launch `json:"launch,omitempty"`
	Interface string `json:"interface,omitempty"` // window | hey-contract
}

// Launch is the entry point for archive/binary kinds, relative to the install
// root; hey resolves it per platform.
type Launch struct {
	Exec string   `json:"exec"`
	Args []string `json:"args,omitempty"`
}

// Kinds.
const (
	KindArchive   = "archive"
	KindAppImage  = "appimage"
	KindBinary    = "binary"
	KindInstaller = "installer"
	KindPackage   = "package"
	KindLink      = "link"
)

// Interfaces.
const (
	InterfaceWindow      = "window"
	InterfaceHeyContract = "hey-contract"
)

// downloadable reports whether a kind names a file hey must fetch and verify.
func downloadable(kind string) bool {
	switch kind {
	case KindArchive, KindAppImage, KindBinary, KindInstaller, KindPackage:
		return true
	default:
		return false
	}
}

// Client is the HTTP client used to fetch manifests. It is a package var so
// tests can point it at an httptest TLS server (whose self-signed cert the
// default client rejects).
var Client = &http.Client{Timeout: 30 * time.Second}

// Fetch downloads and parses the manifest at an https URL.
func Fetch(url string) (*Manifest, error) {
	data, err := FetchBytes(url)
	if err != nil {
		return nil, err
	}
	return Parse(data, url)
}

// FetchBytes downloads the raw bytes at an https URL — a manifest, or its
// detached ".heysig" signature. Signature verification runs over these exact
// bytes, so callers verify before Parse.
func FetchBytes(url string) ([]byte, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("URL must be https: %s", url)
	}
	resp, err := Client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes))
}

// Parse validates raw manifest JSON.
func Parse(data []byte, from string) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest (%s): %w", from, err)
	}
	if m.HeyDeploy == 0 {
		return nil, fmt.Errorf("manifest (%s) is missing hey_deploy", from)
	}
	if m.HeyDeploy > SchemaVersion {
		return nil, fmt.Errorf("manifest (%s) uses hey.deploy.v%d; this hey only understands v%d — update hey", from, m.HeyDeploy, SchemaVersion)
	}
	if m.ID == "" {
		return nil, fmt.Errorf("manifest (%s) is missing id", from)
	}
	if m.Version == "" {
		return nil, fmt.Errorf("manifest (%s) is missing version", from)
	}
	if len(m.Artifacts) == 0 {
		return nil, fmt.Errorf("manifest (%s) lists no artifacts", from)
	}
	for i, a := range m.Artifacts {
		if err := validateArtifact(a); err != nil {
			return nil, fmt.Errorf("manifest (%s) artifact %d: %w", from, i, err)
		}
	}
	return &m, nil
}

func validateArtifact(a Artifact) error {
	if a.Platform == "" {
		return fmt.Errorf("missing platform")
	}
	if a.Kind == "" {
		return fmt.Errorf("missing kind")
	}
	if a.URL == "" {
		return fmt.Errorf("missing url")
	}
	if !strings.HasPrefix(a.URL, "https://") {
		return fmt.Errorf("url must be https, got %q", a.URL)
	}
	if downloadable(a.Kind) && a.SHA256 == "" {
		return fmt.Errorf("kind %q requires a sha256 (hey refuses to run unverified artifacts)", a.Kind)
	}
	return nil
}

// CurrentPlatform maps runtime.GOOS to a manifest platform name.
func CurrentPlatform() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS // windows, linux
}

// CurrentArch maps runtime.GOARCH to a manifest arch name.
func CurrentArch() string {
	if runtime.GOARCH == "amd64" {
		return "x64"
	}
	return runtime.GOARCH // arm64, ...
}

// archMatches reports whether an artifact arch satisfies a target arch. An
// empty or "universal" artifact arch matches anything.
func archMatches(artifactArch, target string) bool {
	return artifactArch == "" || artifactArch == "universal" || artifactArch == target
}

// SelectDesktop picks the artifact for the current desktop machine (platform +
// arch). It errors clearly when nothing matches so a user on an unsupported
// machine gets a real message, not a nil.
func (m *Manifest) SelectDesktop() (Artifact, error) {
	return m.Select(CurrentPlatform(), CurrentArch())
}

// Select returns the artifact matching platform+arch (universal/empty arch is a
// wildcard). Link artifacts are eligible when they name the platform.
func (m *Manifest) Select(platform, arch string) (Artifact, error) {
	for _, a := range m.Artifacts {
		if a.Platform == platform && archMatches(a.Arch, arch) {
			return a, nil
		}
	}
	return Artifact{}, fmt.Errorf("%s: no artifact for %s/%s (has: %s)", m.ID, platform, arch, m.platformSummary())
}

// SelectPackage picks the device-installable package artifact for a mobile
// platform (e.g. "android" → the .apk). Arch is not part of mobile selection.
func (m *Manifest) SelectPackage(platform string) (Artifact, error) {
	for _, a := range m.Artifacts {
		if a.Platform == platform && a.Kind == KindPackage {
			return a, nil
		}
	}
	return Artifact{}, fmt.Errorf("%s: no %s package artifact (has: %s)", m.ID, platform, m.platformSummary())
}

// SelectLink picks a link artifact (a store/TestFlight URL hey just opens). If
// a preferredPlatform is given and present, it wins; otherwise the first link.
func (m *Manifest) SelectLink(preferredPlatform string) (Artifact, error) {
	var first *Artifact
	for i := range m.Artifacts {
		a := m.Artifacts[i]
		if a.Kind != KindLink {
			continue
		}
		if preferredPlatform != "" && a.Platform == preferredPlatform {
			return a, nil
		}
		if first == nil {
			first = &m.Artifacts[i]
		}
	}
	if first != nil {
		return *first, nil
	}
	return Artifact{}, fmt.Errorf("%s: no link artifact to open", m.ID)
}

func (m *Manifest) platformSummary() string {
	seen := map[string]bool{}
	var out []string
	for _, a := range m.Artifacts {
		key := a.Platform
		if a.Arch != "" {
			key += "/" + a.Arch
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return strings.Join(out, ", ")
}
