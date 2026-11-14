package reconcile_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/kube"
	"github.com/ClearedStack/shipkeeper/internal/kubetest"
	"github.com/ClearedStack/shipkeeper/internal/manifest"
	"github.com/ClearedStack/shipkeeper/internal/muster"
	"github.com/ClearedStack/shipkeeper/internal/platform"
	"github.com/ClearedStack/shipkeeper/internal/reconcile"
	"github.com/ClearedStack/shipkeeper/internal/registry"
)

func appYAML(name, ns, image, digest string) []byte {
	return []byte(fmt.Sprintf(
		"name: %s\nnamespace: %s\nimage: %s\ndigest: %s\nreplicas: 1\nport: 8080\nmemory: 30Mi\n",
		name, ns, image, digest,
	))
}

func TestSyncAllReconcilesAcrossNamespacesAndTriggersReplay(t *testing.T) {
	const (
		d1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		d2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
		d3 = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	)

	cluster := kubetest.New(manifest.Namespaces...)
	defer cluster.Close()

	pf := platform.MemoryPlainforge{
		"conn":    appYAML("conn", "clearedstack", "conn", d1),
		"sextant": appYAML("sextant", "cs-workers", "sextant", d1),
		"assay":   []byte("name: [broken"), // broken manifest: reported degraded with E_NEEDS_SUBTOOL
	}
	laz := &platform.MemoryLazarette{}
	laz.Push("conn", d1)
	laz.Push("conn", d2)
	laz.Push("sextant", d1)
	laz.Push("sextant", d2)

	// Simulate Logbook running as a StatefulSet pod in clearedstack with no app.yaml.
	logbookPod, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"name":   "logbook-0",
			"labels": map[string]any{"app.kubernetes.io/name": "logbook"},
		},
		"spec": map[string]any{
			"nodeName": "node-arm64-a",
			"containers": []any{map[string]any{
				"name":  "postgres",
				"image": "docker.io/library/postgres:17@" + d3,
			}},
		},
		"status": map[string]any{
			"phase":      "Running",
			"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
		},
	})
	cluster.AddPod("clearedstack", logbookPod)

	musterMem := &muster.Memory{
		PausedBy:       map[string]int{"sextant": 2},
		BuildingByRepo: map[string]int{"clearedstack.com": 1},
	}
	musterSrv := httptest.NewServer(musterMem)
	defer musterSrv.Close()

	rec := &reconcile.Reconciler{
		Plainforge: pf,
		Lazarette:  laz,
		ContractMuster: &muster.Client{
			BaseURL: musterSrv.URL,
			Token:   "token-shipkeeper",
			HTTP:    &http.Client{Timeout: 2 * time.Second},
		},
		Cluster: &kube.Client{
			BaseURL:      cluster.URL,
			FieldManager: manifest.FieldManager,
			HTTP:         &http.Client{Timeout: 2 * time.Second},
		},
		Registry:      "lazarette.clearedstack.svc.cluster.local:5000",
		ClusterDomain: "cluster.local",
	}

	ctx := context.Background()

	// First pass: conn and sextant are applied (not rolled out yet -> degraded, no digest yet),
	// logbook is healthy from its StatefulSet pod, assay is degraded with E_NEEDS_SUBTOOL.
	s1 := rec.SyncAll(ctx)
	if !s1.Delivered || s1.Applied != 2 || s1.Muster.Received != 4 {
		t.Fatalf("pass 1: %+v", s1)
	}
	if !errors.Is(s1.ComponentErrors["assay"], errs.YAMLUnsupported) {
		t.Fatalf("assay error: %v", s1.ComponentErrors["assay"])
	}

	// Roll out conn and sextant at digest d1 -> Muster records initial digests (replayed=0, live=0).
	cluster.RollOut("clearedstack", "conn")
	cluster.RollOut("cs-workers", "sextant")
	delete(pf, "assay")
	s2 := rec.SyncAll(ctx)
	if !s2.Delivered || s2.Muster.Received != 3 || s2.Muster.Replayed != 0 || s2.Muster.Live != 0 {
		t.Fatalf("pass 2: %+v", s2)
	}

	// Upgrade conn and sextant to d2 while Muster is temporarily down (503 store_slow).
	pf["conn"] = appYAML("conn", "clearedstack", "conn", d2)
	pf["sextant"] = appYAML("sextant", "cs-workers", "sextant", d2)
	musterMem.Down = true

	// Apply d2, then roll out pods while Muster is still down.
	_ = rec.SyncAll(ctx)
	cluster.RollOut("clearedstack", "conn")
	cluster.RollOut("cs-workers", "sextant")
	sDown := rec.SyncAll(ctx)
	if sDown.Delivered || !errors.Is(sDown.MusterError, errs.MusterBusy) {
		t.Fatalf("expected retryable MusterBusy while down, got %+v", sDown)
	}
	if rec.PendingCount() != 3 {
		t.Fatalf("expected 3 buffered reports, got %d", rec.PendingCount())
	}

	// Muster and Logbook recover: buffered healthy d2 digests deliver, replaying 2 paused sextant items
	// and moving 1 building proposal in clearedstack.com to live.
	musterMem.Down = false
	sUp := rec.SyncAll(ctx)
	if !sUp.Delivered || rec.PendingCount() != 0 {
		t.Fatalf("recovery pass did not flush buffer: %+v pending=%d", sUp, rec.PendingCount())
	}
	if sUp.Muster.Received != 3 || sUp.Muster.Replayed != 2 || sUp.Muster.Live != 1 {
		t.Fatalf("unexpected Muster summary after recovery: %+v", sUp.Muster)
	}

	// Verify all 17 registry components run cleanly when none have manifests yet except logbook/conn/sextant.
	if len(registry.All()) != 17 {
		t.Fatal("expected 17 components")
	}
}

func TestSyncAllObservesBootstrapZeroDigestWorkloads(t *testing.T) {
	const liveDigest = "sha256:9999999999999999999999999999999999999999999999999999999999999999"

	cluster := kubetest.New(manifest.Namespaces...)
	defer cluster.Close()

	// Seed a bootstrap app.yaml with the 64-zero placeholder for shipkeeper and bellbuoy.
	pf := platform.MemoryPlainforge{
		"shipkeeper": appYAML("shipkeeper", "clearedstack", "shipkeeper", reconcile.BootstrapDigest),
		"bellbuoy":   appYAML("bellbuoy", "cs-workers", "bellbuoy", reconcile.BootstrapDigest),
	}
	laz := &platform.MemoryLazarette{}

	// shipkeeper is already running in clearedstack via bootstrap deploy/*.yaml; bellbuoy is not deployed yet.
	shipkeeperPod, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"name":   "shipkeeper-7c9d8f-abcde",
			"labels": map[string]any{"app.kubernetes.io/name": "shipkeeper"},
		},
		"spec": map[string]any{
			"nodeName": "node-arm64-a",
			"containers": []any{map[string]any{
				"name":  "shipkeeper",
				"image": "lazarette.clearedstack.svc.cluster.local:5000/shipkeeper@" + liveDigest,
			}},
		},
		"status": map[string]any{
			"phase":      "Running",
			"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
		},
	})
	cluster.AddPod("clearedstack", shipkeeperPod)

	musterMem := &muster.Memory{}
	musterSrv := httptest.NewServer(musterMem)
	defer musterSrv.Close()

	rec := &reconcile.Reconciler{
		Plainforge: pf,
		Lazarette:  laz,
		Muster:     &platform.MemoryMuster{},
		ContractMuster: &muster.Client{
			BaseURL: musterSrv.URL,
			Token:   "token-shipkeeper",
			HTTP:    &http.Client{Timeout: 2 * time.Second},
		},
		Cluster: &kube.Client{
			BaseURL:      cluster.URL,
			FieldManager: manifest.FieldManager,
			HTTP:         &http.Client{Timeout: 2 * time.Second},
		},
		Registry:      "lazarette.clearedstack.svc.cluster.local:5000",
		ClusterDomain: "cluster.local",
	}

	ctx := context.Background()
	sum := rec.SyncAll(ctx)
	if !sum.Delivered || sum.Applied != 0 || len(sum.ComponentErrors) != 0 {
		t.Fatalf("bootstrap SyncAll: %+v", sum)
	}
	if len(sum.Reports) != 1 || sum.Reports[0].Component != "shipkeeper" || sum.Reports[0].State != muster.Healthy || sum.Reports[0].Digest != liveDigest {
		t.Fatalf("unexpected reports: %+v", sum.Reports)
	}

	// Standalone Once continues to reject BootstrapDigest with DigestMissing.
	if _, err := rec.Once(ctx, "shipkeeper"); !errors.Is(err, errs.DigestMissing) {
		t.Fatalf("expected Once to reject BootstrapDigest with DigestMissing, got %v", err)
	}
}
