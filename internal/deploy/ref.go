package deploy

import (
	"fmt"
	"strings"
)

// RefKind classifies how an install/run <ref> resolves to a manifest.
type RefKind int

const (
	// RefScoped is @scope/id — resolved via the registry's scope→URL template.
	RefScoped RefKind = iota
	// RefManifestURL is a direct https manifest URL — used as-is.
	RefManifestURL
	// RefAppName is a bare name — a legacy github-release app in the registry
	// (guten/djin). hey routes these through the existing install/run path so
	// both models coexist under one command surface.
	RefAppName
)

// Ref is a classified install/run reference.
type Ref struct {
	Kind        RefKind
	Scope       string // RefScoped
	ID          string // RefScoped
	ManifestURL string // RefManifestURL
	AppName     string // RefAppName
}

// ParseRef classifies a <ref>. It never fetches; it only decides the route.
//
//   - "@scope/id"                 → RefScoped
//   - "https://…/x.json"          → RefManifestURL
//   - anything else ("guten")     → RefAppName
func ParseRef(s string) (Ref, error) {
	switch {
	case strings.HasPrefix(s, "@"):
		body := s[1:]
		slash := strings.IndexByte(body, '/')
		if slash <= 0 || slash == len(body)-1 {
			return Ref{}, fmt.Errorf("bad scoped ref %q — expected @scope/id", s)
		}
		return Ref{Kind: RefScoped, Scope: body[:slash], ID: body[slash+1:]}, nil
	case strings.HasPrefix(s, "https://"):
		return Ref{Kind: RefManifestURL, ManifestURL: s}, nil
	case strings.HasPrefix(s, "http://"):
		return Ref{}, fmt.Errorf("manifest URL must be https: %s", s)
	default:
		return Ref{Kind: RefAppName, AppName: s}, nil
	}
}
