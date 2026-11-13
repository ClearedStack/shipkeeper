package kube_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/kube"
	"github.com/ClearedStack/shipkeeper/internal/kubetest"
)

const deployment = `{"apiVersion":"apps/v1","kind":"Deployment",
 "metadata":{"name":"example-app","namespace":"cs-workers","labels":{"app.kubernetes.io/name":"example-app"}},
 "spec":{"replicas":2,"template":{"spec":{"containers":[{"name":"example-app"}]}}}}`

func object(body string) kube.Object {
	return kube.Object{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "cs-workers", Name: "example-app", Body: []byte(body)}
}

func setup(t *testing.T) (*kubetest.Server, *kube.Client) {
	t.Helper()
	srv := kubetest.New("clearedstack", "cs-workers")
	t.Cleanup(srv.Close)
	return srv, &kube.Client{BaseURL: srv.URL, FieldManager: "shipkeeper", HTTP: &http.Client{Timeout: 2 * time.Second}}
}

func TestApplyCreatesThenIsIdempotent(t *testing.T) {
	srv, c := setup(t)
	ctx := context.Background()
	if err := c.Apply(ctx, object(deployment), true); err != nil {
		t.Fatal(err)
	}
	if srv.Field("cs-workers", "example-app", "spec.replicas") != nil {
		t.Fatal("a dry run stored the object")
	}
	for i := 0; i < 2; i++ {
		if err := c.Apply(ctx, object(deployment), false); err != nil {
			t.Fatal(err)
		}
	}
	if got := srv.Field("cs-workers", "example-app", "spec.replicas"); got != 2.0 {
		t.Fatalf("replicas stored as %v", got)
	}
	obs, err := c.Observe(ctx, "cs-workers", "example-app", "app.kubernetes.io/name")
	if err != nil {
		t.Fatal(err)
	}
	if obs.Deployment.Metadata.Generation != 1 {
		t.Errorf("applying the same object twice changed the generation to %d", obs.Deployment.Metadata.Generation)
	}
}

func TestApplyDropsFieldsNoLongerSet(t *testing.T) {
	srv, c := setup(t)
	ctx := context.Background()
	if err := c.Apply(ctx, object(deployment), false); err != nil {
		t.Fatal(err)
	}
	without := strings.Replace(deployment, `"replicas":2,`, ``, 1)
	if err := c.Apply(ctx, object(without), false); err != nil {
		t.Fatal(err)
	}
	if got := srv.Field("cs-workers", "example-app", "spec.replicas"); got != nil {
		t.Errorf("a field Shipkeeper stopped setting is still there: %v", got)
	}
}

// Failure mode "Server-Side Apply conflict": a manual edit owns a field
// Shipkeeper sets. The apply is refused and the edit is left alone.
func TestApplyConflictIsReportedAndNothingOverwritten(t *testing.T) {
	srv, c := setup(t)
	ctx := context.Background()
	if err := c.Apply(ctx, object(deployment), false); err != nil {
		t.Fatal(err)
	}
	srv.Edit("cs-workers", "example-app", "someone-with-kubectl", "spec.replicas", 5)

	for _, dryRun := range []bool{true, false} {
		err := c.Apply(ctx, object(deployment), dryRun)
		if !errors.Is(err, errs.ApplyConflict) {
			t.Fatalf("dryRun=%v: got %v, want a conflict", dryRun, err)
		}
		var e *errs.Error
		errors.As(err, &e)
		if !reflect.DeepEqual(e.Fields, []string{".spec.replicas"}) {
			t.Errorf("conflicting fields: %v", e.Fields)
		}
		if strings.Contains(err.Error(), "someone-with-kubectl") {
			t.Errorf("the error names the other manager: %v", err)
		}
	}
	if got := srv.Field("cs-workers", "example-app", "spec.replicas"); got != 5.0 {
		t.Errorf("the manual edit was overwritten: replicas is %v", got)
	}

	// When the manual edit agrees with app.yaml there is nothing to fight over.
	srv.Edit("cs-workers", "example-app", "someone-with-kubectl", "spec.replicas", 2)
	if err := c.Apply(ctx, object(deployment), false); err != nil {
		t.Errorf("same value, still refused: %v", err)
	}
}

func TestErrorsComeFromTheCatalogueAndNameNothing(t *testing.T) {
	ctx := context.Background()
	check := func(name string, err error, want *errs.Error, srv *kubetest.Server) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", name, err, want)
			return
		}
		host := strings.TrimPrefix(srv.URL, "http://")
		addr, port, _ := strings.Cut(host, ":")
		for _, secret := range []string{host, addr, port, "example-app", "secret-token"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: error text contains %q: %v", name, secret, err)
			}
		}
	}

	srv, c := setup(t)
	srv.Delay = 300 * time.Millisecond
	c.HTTP.Timeout = 50 * time.Millisecond
	check("slow apply", c.Apply(ctx, object(deployment), false), errs.KubeTimeout, srv)
	_, err := c.Observe(ctx, "cs-workers", "example-app", "k")
	check("slow read", err, errs.KubeTimeout, srv)

	srv, c = setup(t)
	srv.Token = "secret-token"
	c.Token = "wrong"
	check("bad token", c.Apply(ctx, object(deployment), false), errs.KubeRefused, srv)
	c.Token = "secret-token"
	if err := c.Apply(ctx, object(deployment), false); err != nil {
		t.Errorf("good token: %v", err)
	}

	srv, c = setup(t)
	if err := c.Apply(ctx, object(deployment), false); err != nil {
		t.Fatal(err)
	}
	srv.Garbage = true
	_, err = c.Observe(ctx, "cs-workers", "example-app", "k")
	check("reply that is not JSON", err, errs.KubeBadReply, srv)

	srv, c = setup(t)
	_, err = c.Observe(ctx, "cs-workers", "example-app", "k")
	check("deployment not there", err, errs.KubeUnexpected, srv)
	other := object(deployment)
	other.Namespace = "kube-system"
	check("namespace not there", c.Apply(ctx, other, false), errs.KubeUnexpected, srv)
	cm := object(deployment)
	cm.APIVersion, cm.Kind = "v1", "ConfigMap"
	check("kind not managed", c.Apply(ctx, cm, false), errs.KubeKind, srv)

	srv, c = setup(t)
	srv.Close()
	check("server gone", c.Apply(ctx, object(deployment), false), errs.KubeUnreachable, srv)
}
