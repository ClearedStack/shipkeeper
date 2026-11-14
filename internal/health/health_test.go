package health

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/golden"
	"github.com/ClearedStack/shipkeeper/internal/kube"
)

var cfg = Config{ClusterDomain: "cluster.local", Namespaces: []string{"clearedstack", "cs-workers"}}

// Every directory under testdata is one saved example: observed.json is
// what was read from the cluster, and report.json is the exact report sent
// on, or error.txt the exact refusal. The directory name starts with the
// state the report must carry, or with "refused".
func TestSavedExamples(t *testing.T) {
	dirs, _ := filepath.Glob("testdata/*")
	if len(dirs) == 0 {
		t.Fatal("no saved examples found")
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, "observed.json"))
			if err != nil {
				t.Fatal(err)
			}
			var obs kube.Observed
			if err := json.Unmarshal(raw, &obs); err != nil {
				t.Fatal(err)
			}
			want, _, _ := strings.Cut(filepath.Base(dir), "-")
			report, err := Build("example-app", "cs-workers", obs, cfg)
			if err != nil {
				if want != "refused" {
					t.Fatalf("refused: %v", err)
				}
				if _, catalogued := err.(*errs.Error); !catalogued {
					t.Fatalf("error is not from the catalogue: %v", err)
				}
				golden.Equal(t, filepath.Join(dir, "error.txt"), []byte(err.Error()+"\n"))
				return
			}
			if string(report.State) != want {
				t.Errorf("state is %q, the example is named for %q", report.State, want)
			}
			out, _ := json.MarshalIndent(report, "", "  ")
			out = append(out, '\n')
			golden.Equal(t, filepath.Join(dir, "report.json"), out)

			// Whatever the saved report says, nothing that names a node, a
			// pod or an address in the input may be in the output.
			for _, secret := range secrets(obs) {
				if strings.Contains(string(out), secret) {
					t.Errorf("report still contains %q", secret)
				}
			}
		})
	}
}

func secrets(obs kube.Observed) []string {
	var s []string
	for _, p := range obs.Pods {
		s = append(s, p.Metadata.Name, p.Spec.NodeName, p.Spec.Hostname, p.Status.NominatedNodeName, p.Status.HostIP, p.Status.PodIP)
		for _, a := range append(p.Status.HostIPs, p.Status.PodIPs...) {
			s = append(s, a.IP)
		}
	}
	for _, e := range obs.Events {
		s = append(s, e.Source.Host, e.ReportingInstance)
	}
	var out []string
	for _, v := range s {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func TestSameInputSameReport(t *testing.T) {
	raw, _ := os.ReadFile("testdata/degraded-crash-loop/observed.json")
	var obs kube.Observed
	json.Unmarshal(raw, &obs)
	first, _ := Build("example-app", "cs-workers", obs, cfg)
	a, _ := json.Marshal(first)
	// The order pods and events arrive in must not matter.
	obs.Pods[0], obs.Pods[1] = obs.Pods[1], obs.Pods[0]
	for i, j := 0, len(obs.Events)-1; i < j; i, j = i+1, j-1 {
		obs.Events[i], obs.Events[j] = obs.Events[j], obs.Events[i]
	}
	second, _ := Build("example-app", "cs-workers", obs, cfg)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Errorf("reports differ:\n%s\n%s", a, b)
	}
}
