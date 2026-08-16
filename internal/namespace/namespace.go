// Package namespace unifies the three name namespaces hey validates
// separately today — internal/registry.Reserved (CLI commands, which also
// double as reserved app names), internal/svc pack names, and the hub's
// service-instance names (hub-known names, plus any user-chosen `hey up
// <pack> --name <instance>`) — into one collision check.
//
// docs/design/services.md §10 open question 5 names the gap this closes:
// "nothing stops a pack named e.g. `ps` or an app named `postgres` from
// shadowing a hub-known name... These need one unified reservation/collision
// check before `hey up` ships, not three independently-validated lists that
// can silently disagree." A hub service's Name() is always either a pack
// name (svc lifecycle) or an app name (contract lifecycle, future) — so
// checking Reserved ∪ registry app names ∪ pack names for internal overlap
// *is* the unified check: nothing on the hub can be named something that
// isn't already exactly one of those two sources.
package namespace

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kitsyai/hey/internal/registry"
	"github.com/kitsyai/hey/internal/svc"
)

// source labels where a name came from, for a clear collision message.
type source struct {
	kind string // "reserved command", "app", "pack"
	name string
}

// CheckCollisions verifies every name across the three namespaces resolves
// to exactly one source. extra are additional names to fold in under the
// given kind label (e.g. a hub catalog name that isn't 1:1 with a pack or
// app, or a user's `--name` for a not-yet-provisioned instance) — pass none
// for a pure catalog check.
func CheckCollisions(reg *registry.Registry, packs *svc.PackSet, extra map[string]string) error {
	claims := map[string][]source{}
	add := func(kind, name string) {
		claims[name] = append(claims[name], source{kind: kind, name: name})
	}
	for _, r := range registry.Reserved {
		add("reserved command", r)
	}
	if reg != nil {
		for _, n := range reg.Names() {
			add("app", n)
		}
	}
	if packs != nil {
		for _, n := range packs.Names() {
			add("pack", n)
		}
	}
	// Deterministic order for a stable error on multiple collisions.
	extraNames := make([]string, 0, len(extra))
	for n := range extra {
		extraNames = append(extraNames, n)
	}
	sort.Strings(extraNames)
	for _, n := range extraNames {
		add(extra[n], n)
	}

	names := make([]string, 0, len(claims))
	for n := range claims {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		srcs := claims[n]
		if len(srcs) < 2 {
			continue
		}
		kinds := make([]string, len(srcs))
		for i, s := range srcs {
			kinds[i] = s.kind
		}
		return fmt.Errorf(
			"namespace collision: %q is claimed by both %s — hey requires every "+
				"reserved command, app, pack, and hub-service name to be unique across "+
				"all of them (see docs/design/services.md §10 open question 5)",
			n, strings.Join(kinds, " and "))
	}
	return nil
}

// CheckName validates a single candidate instance name (e.g. a user-supplied
// `hey up <selfPack> --name <instance>`) against Reserved, the registry's app
// names, and every OTHER pack's name. selfPack is the pack being instantiated
// under this name — naming an instance after its own pack (the default, e.g.
// "postgres" instance of the "postgres" pack) is not a collision; naming one
// pack's instance after a *different* pack (or app) is, since the hub keys
// its /services entries by this same name and two different services can
// never share a key.
func CheckName(name, selfPack string, reg *registry.Registry, packs *svc.PackSet) error {
	if name == "" {
		return fmt.Errorf("name must not be empty")
	}
	for _, r := range registry.Reserved {
		if name == r {
			return fmt.Errorf("name %q collides with a reserved hey command", name)
		}
	}
	if reg != nil {
		for _, n := range reg.Names() {
			if name == n {
				return fmt.Errorf("name %q collides with an installed app", name)
			}
		}
	}
	if packs != nil {
		for _, n := range packs.Names() {
			if name == n && name != selfPack {
				return fmt.Errorf("name %q collides with a different pack (%q) — pick another --name", name, n)
			}
		}
	}
	return nil
}
