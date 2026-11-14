package reconcile_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/example"
	"github.com/ClearedStack/shipkeeper/internal/health"
	"github.com/ClearedStack/shipkeeper/internal/kube"
	"github.com/ClearedStack/shipkeeper/internal/manifest"
	"github.com/ClearedStack/shipkeeper/internal/platform"
	"github.com/ClearedStack/shipkeeper/internal/reconcile"
)

// TestRealAPIServer runs the milestone against a real Kubernetes API
// server. It is skipped unless SHIPKEEPER_TEST_KUBE_URL is set, so the
// ordinary test run needs no cluster and no network.
//
// Also set SHIPKEEPER_TEST_KUBE_TOKEN_FILE and SHIPKEEPER_TEST_KUBE_CA_FILE.
// The namespace cs-workers must exist, and the Deployment, Service and
// NetworkPolicy called example-app in it must not. Use a throwaway API
// server: the test leaves those three objects behind.
func TestRealAPIServer(t *testing.T) {
	url := os.Getenv("SHIPKEEPER_TEST_KUBE_URL")
	if url == "" {
		t.Skip("SHIPKEEPER_TEST_KUBE_URL is not set")
	}
	token, err := os.ReadFile(os.Getenv("SHIPKEEPER_TEST_KUBE_TOKEN_FILE"))
	if err != nil {
		t.Fatal("cannot read the token file")
	}
	pem, err := os.ReadFile(os.Getenv("SHIPKEEPER_TEST_KUBE_CA_FILE"))
	pool := x509.NewCertPool()
	if err != nil || !pool.AppendCertsFromPEM(pem) {
		t.Fatal("cannot read the CA file")
	}
	client := &kube.Client{
		BaseURL: url, Token: strings.TrimSpace(string(token)), FieldManager: manifest.FieldManager,
		HTTP: &http.Client{Timeout: 10 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}},
	}
	lazarette := &platform.MemoryLazarette{}
	for image, digests := range example.Digests() {
		for _, d := range digests {
			lazarette.Push(image, d)
		}
	}
	muster := &platform.MemoryMuster{}
	r := &reconcile.Reconciler{
		Plainforge: platform.MemoryPlainforge{example.App: example.File("app.yaml")},
		Lazarette:  lazarette, Muster: muster, Cluster: client,
		Registry: "lazarette.clearedstack.svc.cluster.local:5000", ClusterDomain: "cluster.local",
	}
	ctx := context.Background()

	// Apply, then apply again: the second pass must change nothing.
	for pass := 1; pass <= 2; pass++ {
		res, err := r.Once(ctx, example.App)
		if err != nil || !res.Applied || !res.Delivered {
			t.Fatalf("pass %d: %v, %+v", pass, err, res)
		}
	}
	obs, err := client.Observe(ctx, example.Namespace, example.App, manifest.SelectorLabel)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Deployment.Metadata.Generation != 1 {
		t.Errorf("generation after two identical applies: %d", obs.Deployment.Metadata.Generation)
	}
	if obs.Deployment.Spec.Replicas == nil || *obs.Deployment.Spec.Replicas != 2 {
		t.Errorf("replicas read back: %v", obs.Deployment.Spec.Replicas)
	}
	t.Logf("state reported: %s", muster.Reports[len(muster.Reports)-1].State)
	if s := muster.Reports[len(muster.Reports)-1].State; s != health.Degraded && s != health.Healthy {
		t.Errorf("state: %s", s)
	}

	// Someone else takes the replica count. Shipkeeper must report the
	// conflict and leave their value in place.
	body := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"example-app","namespace":"cs-workers"},"spec":{"replicas":7}}`
	req, _ := http.NewRequest(http.MethodPatch,
		url+"/apis/apps/v1/namespaces/cs-workers/deployments/example-app?fieldManager=someone-else&force=true",
		bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/apply-patch+yaml")
	req.Header.Set("Authorization", "Bearer "+client.Token)
	resp, err := client.HTTP.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("could not make the out-of-band edit: %v %v", err, resp)
	}
	resp.Body.Close()

	res, err := r.Once(ctx, example.App)
	if !errors.Is(err, errs.ApplyConflict) || res.Applied {
		t.Fatalf("got %v, %+v; want a conflict", err, res)
	}
	var e *errs.Error
	errors.As(err, &e)
	if !reflect.DeepEqual(e.Fields, []string{".spec.replicas"}) {
		t.Errorf("conflicting fields: %v", e.Fields)
	}
	obs, err = client.Observe(ctx, example.Namespace, example.App, manifest.SelectorLabel)
	if err != nil {
		t.Fatal(err)
	}
	if *obs.Deployment.Spec.Replicas != 7 {
		t.Errorf("the out-of-band edit was overwritten: replicas is %d", *obs.Deployment.Spec.Replicas)
	}
}
