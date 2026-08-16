package namespace

import (
	"strings"
	"testing"

	"github.com/kitsyai/hey/internal/registry"
	"github.com/kitsyai/hey/internal/svc"
)

func testRegistry(apps ...string) *registry.Registry {
	r := &registry.Registry{Apps: map[string]registry.App{}}
	for _, a := range apps {
		r.Apps[a] = registry.App{Source: registry.Source{Type: "github-release", Repo: "x/y", AssetTemplate: "a", ChecksumsAsset: "b", Binary: "c"}}
	}
	return r
}

func testPacks(names ...string) *svc.PackSet {
	ps := &svc.PackSet{Packs: map[string]svc.Pack{}}
	for _, n := range names {
		ps.Packs[n] = svc.Pack{Pack: n}
	}
	return ps
}

func TestCheckCollisions_Clean(t *testing.T) {
	reg := testRegistry("guten")
	packs := testPacks("postgres", "vinci")
	if err := CheckCollisions(reg, packs, nil); err != nil {
		t.Fatalf("expected no collision, got %v", err)
	}
}

func TestCheckCollisions_PackShadowsReserved(t *testing.T) {
	reg := testRegistry("guten")
	packs := testPacks("postgres", "ps") // "ps" is a reserved hey command
	err := CheckCollisions(reg, packs, nil)
	if err == nil {
		t.Fatal("expected a collision error")
	}
	if !strings.Contains(err.Error(), `"ps"`) {
		t.Errorf("error should name the colliding name: %v", err)
	}
}

func TestCheckCollisions_AppShadowsPack(t *testing.T) {
	reg := testRegistry("postgres") // an app literally named "postgres"
	packs := testPacks("postgres")
	err := CheckCollisions(reg, packs, nil)
	if err == nil || !strings.Contains(err.Error(), "app") || !strings.Contains(err.Error(), "pack") {
		t.Fatalf("expected an app/pack collision error, got %v", err)
	}
}

func TestCheckCollisions_ExtraName(t *testing.T) {
	reg := testRegistry()
	packs := testPacks("postgres")
	err := CheckCollisions(reg, packs, map[string]string{"postgres": "hub-service"})
	if err == nil {
		t.Fatal("expected a collision between the pack and the extra hub-service name")
	}
}

func TestCheckName_SelfNameOK(t *testing.T) {
	reg := testRegistry()
	packs := testPacks("postgres")
	if err := CheckName("postgres", "postgres", reg, packs); err != nil {
		t.Errorf("naming an instance after its own pack should be fine: %v", err)
	}
}

func TestCheckName_ReservedRejected(t *testing.T) {
	reg := testRegistry()
	packs := testPacks("postgres")
	if err := CheckName("up", "postgres", reg, packs); err == nil {
		t.Error("expected reserved-name rejection")
	}
}

func TestCheckName_CrossPackRejected(t *testing.T) {
	reg := testRegistry()
	packs := testPacks("postgres", "vinci")
	if err := CheckName("vinci", "postgres", reg, packs); err == nil {
		t.Error("expected cross-pack name collision to be rejected")
	}
}

func TestCheckName_AppRejected(t *testing.T) {
	reg := testRegistry("guten")
	packs := testPacks("postgres")
	if err := CheckName("guten", "postgres", reg, packs); err == nil {
		t.Error("expected app-name collision to be rejected")
	}
}
