package sanitize

import (
	"encoding/json"
	"os"
	"testing"
)

// testdata/cases.json holds one saved input and its exact output for every
// rule, all run with the cluster knowledge in testdata/config.json.
func TestSavedExamples(t *testing.T) {
	var cfg Config
	load(t, "testdata/config.json", &cfg)
	var cases []struct{ Rule, In, Out string }
	load(t, "testdata/cases.json", &cases)
	if len(cases) == 0 {
		t.Fatal("no saved examples")
	}
	s := New(cfg)
	for _, c := range cases {
		t.Run(c.Rule, func(t *testing.T) {
			got := s.Text(c.In)
			if got != c.Out {
				t.Errorf("\n in:   %q\n got:  %q\n want: %q", c.In, got, c.Out)
			}
			if s.Leaks(got) {
				t.Errorf("cleaning %q a second time changed it again", got)
			}
			if c.In != c.Out && !s.Leaks(c.In) {
				t.Errorf("Leaks did not flag %q", c.In)
			}
		})
	}
}

func TestNoKnowledge(t *testing.T) {
	s := New(Config{})
	if got := s.Text("node-a at 10.0.0.1 svc.cluster.local"); got != "node-a at [address] svc.cluster.local" {
		t.Errorf("got %q", got)
	}
}

func load(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}
