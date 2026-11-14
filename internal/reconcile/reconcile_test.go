package reconcile_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/example"
	"github.com/ClearedStack/shipkeeper/internal/golden"
	"github.com/ClearedStack/shipkeeper/internal/health"
	"github.com/ClearedStack/shipkeeper/internal/kube"
	"github.com/ClearedStack/shipkeeper/internal/kubetest"
	"github.com/ClearedStack/shipkeeper/internal/manifest"
	"github.com/ClearedStack/shipkeeper/internal/platform"
	"github.com/ClearedStack/shipkeeper/internal/reconcile"
)

type world struct {
	cluster    *kubetest.Server
	plainforge platform.MemoryPlainforge
	lazarette  *platform.MemoryLazarette
	muster     *platform.MemoryMuster
	r          *reconcile.Reconciler
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{
		cluster:    kubetest.New(manifest.Namespaces...),
		plainforge: platform.MemoryPlainforge{example.App: example.File("app.yaml")},
		lazarette:  &platform.MemoryLazarette{},
		muster:     &platform.MemoryMuster{},
	}
	t.Cleanup(w.cluster.Close)
	for image, digests := range example.Digests() {
		for _, d := range digests {
			w.lazarette.Push(image, d)
		}
	}
	w.r = &reconcile.Reconciler{
		Plainforge: w.plainforge, Lazarette: w.lazarette, Muster: w.muster,
		Cluster: &kube.Client{BaseURL: w.cluster.URL, FieldManager: manifest.FieldManager,
			HTTP: &http.Client{Timeout: 2 * time.Second}},
		Registry: "lazarette.clearedstack.svc.cluster.local:5000", ClusterDomain: "cluster.local",
	}
	return w
}

func (w *world) requests() []byte {
	var b strings.Builder
	for _, r := range w.cluster.Requests() {
		kind := ""
		if r.DryRun {
			kind = " (dry run)"
		}
		fmt.Fprintf(&b, "%s %s%s\n", r.Method, r.Path, kind)
	}
	return []byte(b.String())
}

func reportJSON(r health.Report) []byte {
	out, _ := json.MarshalIndent(r, "", "  ")
	return append(out, '\n')
}

// The milestone, end to end, as saved examples: the example app.yaml goes
// in; the exact requests made to the cluster and the exact reports sent to
// Muster come out.
func TestMilestoneEndToEnd(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	res, err := w.r.Once(ctx, example.App)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || !res.Delivered {
		t.Fatalf("result: %+v", res)
	}
	golden.Equal(t, "testdata/requests.txt", w.requests())
	golden.Equal(t, "testdata/report-just-applied.json", reportJSON(w.muster.Reports[0]))
	if got := w.cluster.Field(example.Namespace, example.App, "spec.replicas"); got != 2.0 {
		t.Errorf("replicas in the cluster: %v", got)
	}

	// The cluster does its part: two pods, one of them crash-looping.
	for _, p := range example.Objects("pods.json") {
		w.cluster.AddPod(example.Namespace, p)
	}
	for _, e := range example.Objects("events.json") {
		w.cluster.AddEvent(example.Namespace, e)
	}
	w.cluster.SetDeploymentStatus(example.Namespace, example.App, map[string]any{
		"observedGeneration": 1, "replicas": 2, "updatedReplicas": 2, "readyReplicas": 1, "availableReplicas": 1})
	if _, err := w.r.Once(ctx, example.App); err != nil {
		t.Fatal(err)
	}
	degraded := reportJSON(w.muster.Reports[1])
	golden.Equal(t, "testdata/report-degraded.json", degraded)

	// Nothing from the raw pods and events that names a node, a pod or an
	// address may be in what Muster received.
	for _, secret := range []string{
		"worker-node", "rack", "example.internal", "10.20.30.", "10.42.", "fd00:",
		"q4zt2", "h8kxw", "svc.cluster.local", "other-app",
	} {
		if strings.Contains(string(degraded), secret) {
			t.Errorf("the report sent to Muster contains %q", secret)
		}
	}

	w.cluster.RollOut(example.Namespace, example.App)
	if _, err := w.r.Once(ctx, example.App); err != nil {
		t.Fatal(err)
	}
	if got := w.muster.Reports[2].State; got != health.Healthy {
		t.Errorf("state after the rollout finished: %s", got)
	}
}

// Failure mode "Missing or unsigned digest": the rollout is blocked and
// what is running is left as it is.
func TestMissingDigestBlocksTheRollout(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if _, err := w.r.Once(ctx, example.App); err != nil {
		t.Fatal(err)
	}
	before := len(w.cluster.Requests())

	unsigned := strings.Replace(string(example.File("app.yaml")), "sha256:4e7a", "sha256:ffff", 1)
	w.plainforge[example.App] = []byte(unsigned)
	res, err := w.r.Once(ctx, example.App)
	if !errors.Is(err, errs.DigestMissing) || res.Applied {
		t.Fatalf("got %v, %+v", err, res)
	}
	if after := len(w.cluster.Requests()); after != before {
		t.Errorf("%d requests reached the cluster for an unsigned digest", after-before)
	}
}

// Lazarette keeps five versions. A digest pushed out by newer ones is no
// longer accepted.
func TestOnlyRetainedDigestsAreAccepted(t *testing.T) {
	w := newWorld(t)
	for i := 0; i < platform.Retained; i++ {
		w.lazarette.Push(example.App, fmt.Sprintf("sha256:%064d", i))
	}
	_, err := w.r.Once(context.Background(), example.App)
	if !errors.Is(err, errs.DigestMissing) {
		t.Fatalf("got %v", err)
	}
	if n := len(w.cluster.Requests()); n != 0 {
		t.Errorf("%d requests reached the cluster", n)
	}
}

// Failure mode "Server-Side Apply conflict", end to end: the dry run finds
// the conflict, so not even the objects that do not conflict are written.
func TestConflictStopsBeforeAnyWrite(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if _, err := w.r.Once(ctx, example.App); err != nil {
		t.Fatal(err)
	}
	writes := w.cluster.Writes()
	w.cluster.Edit(example.Namespace, example.App, "someone-with-kubectl", "spec.replicas", 7)

	res, err := w.r.Once(ctx, example.App)
	if !errors.Is(err, errs.ApplyConflict) || res.Applied {
		t.Fatalf("got %v, %+v", err, res)
	}
	if got := w.cluster.Writes(); got != writes {
		t.Errorf("%d objects were written despite the conflict", got-writes)
	}
	if got := w.cluster.Field(example.Namespace, example.App, "spec.replicas"); got != 7.0 {
		t.Errorf("the manual edit was overwritten: replicas is %v", got)
	}
	if len(w.muster.Reports) != 1 {
		t.Errorf("a report was sent for a pass that stopped")
	}
}

// Failure mode "Hub API unreachable", as far as this milestone goes: the
// apply still happens and the failure to deliver is reported. Keeping the
// report and retrying later is not built yet.
func TestMusterDownDoesNotStopTheApply(t *testing.T) {
	w := newWorld(t)
	w.muster.Down = true
	res, err := w.r.Once(context.Background(), example.App)
	if !errors.Is(err, errs.MusterTimeout) {
		t.Fatalf("got %v", err)
	}
	if !res.Applied || res.Report == nil || res.Delivered {
		t.Errorf("result: %+v", res)
	}
	if w.cluster.Writes() != 3 {
		t.Errorf("writes: %d", w.cluster.Writes())
	}
}

func TestRefusals(t *testing.T) {
	ctx := context.Background()

	w := newWorld(t)
	if _, err := w.r.Once(ctx, "no-such-app"); !errors.Is(err, errs.PlainforgeNoApp) {
		t.Errorf("unknown app: %v", err)
	}

	w = newWorld(t)
	w.plainforge["another-name"] = example.File("app.yaml")
	if _, err := w.r.Once(ctx, "another-name"); !errors.Is(err, errs.ManifestOtherApp) {
		t.Errorf("app.yaml for a different app: %v", err)
	}

	w = newWorld(t)
	w.plainforge[example.App] = []byte("name: [broken")
	if _, err := w.r.Once(ctx, example.App); errs.CodeOf(err) != errs.NeedsSubtool {
		t.Errorf("unsupported YAML: %v", err)
	}

	w = newWorld(t)
	w.cluster.Delay = 300 * time.Millisecond
	w.r.Cluster.(*kube.Client).HTTP.Timeout = 50 * time.Millisecond
	if _, err := w.r.Once(ctx, example.App); !errors.Is(err, errs.KubeTimeout) {
		t.Errorf("slow cluster: %v", err)
	}
	if n := len(w.muster.Reports); n != 0 {
		t.Errorf("%d reports sent by a pass that was refused", n)
	}
}
