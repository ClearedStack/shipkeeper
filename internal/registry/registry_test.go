package registry_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/ClearedStack/shipkeeper/internal/registry"
)

func TestRegistryCountsAndSelfEntry(t *testing.T) {
	all := registry.All()
	if len(all) != 17 {
		t.Fatalf("got %d components, want 17", len(all))
	}
	var control, workers int
	seen := map[string]bool{}
	for _, c := range all {
		if seen[c.Slug] {
			t.Fatalf("duplicate slug %q", c.Slug)
		}
		seen[c.Slug] = true
		switch c.Kind {
		case registry.ControlPlane:
			control++
			if c.Namespace != registry.ControlNamespace {
				t.Errorf("%s: namespace %q, want %q", c.Slug, c.Namespace, registry.ControlNamespace)
			}
		case registry.Worker:
			workers++
			if c.Namespace != registry.WorkerNamespace {
				t.Errorf("%s: namespace %q, want %q", c.Slug, c.Namespace, registry.WorkerNamespace)
			}
		default:
			t.Errorf("%s: unexpected kind %q", c.Slug, c.Kind)
		}
	}
	if control != 7 || workers != 10 {
		t.Fatalf("got %d control-plane and %d workers, want 7 and 10", control, workers)
	}

	raw, err := os.ReadFile("../../component.json")
	if err != nil {
		t.Fatal(err)
	}
	var own struct {
		Slug       string `json:"slug"`
		Kind       string `json:"kind"`
		Namespace  string `json:"namespace"`
		Repository string `json:"repository"`
		Image      string `json:"image"`
	}
	if err := json.Unmarshal(raw, &own); err != nil {
		t.Fatal(err)
	}
	self, ok := registry.Lookup(registry.Shipkeeper)
	if !ok {
		t.Fatal("shipkeeper missing from registry")
	}
	if own.Slug != self.Slug || own.Kind != string(self.Kind) || own.Namespace != self.Namespace ||
		own.Repository != self.Repository || own.Image != self.Image {
		t.Fatalf("component.json %+v disagrees with registry %+v", own, self)
	}
}
