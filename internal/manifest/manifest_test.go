package manifest

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/golden"
)

const registry = "lazarette.clearedstack.svc.cluster.local:5000"

// Every directory under testdata is one saved example: app.yaml goes in,
// and either objects.json (the three objects, in apply order) or error.txt
// (the exact refusal) comes out. Directories named ok-* must succeed and
// all others must be refused.
func TestSavedExamples(t *testing.T) {
	dirs, err := filepath.Glob("testdata/*")
	if err != nil || len(dirs) == 0 {
		t.Fatal("no saved examples found")
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(dir, "app.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			wantOK := strings.HasPrefix(filepath.Base(dir), "ok-")
			app, err := Parse(data)
			if err != nil {
				if wantOK {
					t.Fatalf("refused: %v", err)
				}
				if _, catalogued := err.(*errs.Error); !catalogued {
					t.Fatalf("error is not from the catalogue: %v", err)
				}
				golden.Equal(t, filepath.Join(dir, "error.txt"), []byte(err.Error()+"\n"))
				return
			}
			if !wantOK {
				t.Fatal("accepted, but this example must be refused")
			}
			objects, err := Render(app, registry)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			for _, o := range objects {
				out.Write(o.Body)
			}
			golden.Equal(t, filepath.Join(dir, "objects.json"), out.Bytes())
		})
	}
}

func TestRenderIsRepeatable(t *testing.T) {
	data, _ := os.ReadFile("testdata/ok-full/app.yaml")
	app, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := Render(app, registry)
	for i := 0; i < 20; i++ {
		again, _ := Render(app, registry)
		for j := range first {
			if !bytes.Equal(first[j].Body, again[j].Body) {
				t.Fatal("the same app rendered to different bytes")
			}
		}
	}
}

func TestRenderRefusesBadRegistry(t *testing.T) {
	for _, r := range []string{"", "https://registry.example", "registry.example/path", "Registry.Example"} {
		if _, err := Render(App{}, r); err != errs.Config {
			t.Errorf("registry %q: got %v, want %v", r, err, errs.Config)
		}
	}
}
