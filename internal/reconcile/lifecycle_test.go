package reconcile_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/health"
	"github.com/ClearedStack/shipkeeper/internal/kube"
	"github.com/ClearedStack/shipkeeper/internal/kubetest"
	"github.com/ClearedStack/shipkeeper/internal/manifest"
	"github.com/ClearedStack/shipkeeper/internal/muster"
	"github.com/ClearedStack/shipkeeper/internal/platform"
	"github.com/ClearedStack/shipkeeper/internal/reconcile"
)

func TestLifecycleUpgradePauseResumeRollback(t *testing.T) {
	const (
		d1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		d2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
		d3 = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
		d4 = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
		d5 = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
		d6 = "sha256:6666666666666666666666666666666666666666666666666666666666666666"
	)

	cluster := kubetest.New(manifest.Namespaces...)
	defer cluster.Close()

	// Start sextant on the bootstrap zero-digest placeholder, then upgrade into retained digests.
	pf := platform.MemoryPlainforge{
		"sextant": appYAML("sextant", "cs-workers", "sextant", reconcile.BootstrapDigest),
	}
	laz := &platform.MemoryLazarette{}
	laz.Push("sextant", d1)
	laz.Push("sextant", d2)
	laz.Push("sextant", d3)

	localMuster := &platform.MemoryMuster{}
	musterMem := &muster.Memory{}
	musterSrv := httptest.NewServer(musterMem)
	defer musterSrv.Close()

	rec := &reconcile.Reconciler{
		Plainforge: pf,
		Lazarette:  laz,
		Muster:     localMuster,
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

	// 1. Upgrade pins sextant to newest retained digest d3.
	upRes, err := rec.Upgrade(ctx, "sextant")
	if err != nil || !upRes.Applied || !upRes.Delivered || upRes.Report == nil {
		t.Fatalf("Upgrade: res=%+v err=%v", upRes, err)
	}
	cur, err := manifest.Parse(pf["sextant"])
	if err != nil || cur.Digest != d3 || cur.Replicas != 1 {
		t.Fatalf("manifest after Upgrade: %+v err=%v", cur, err)
	}

	// 2. Pause sets replicas: 0 in app.yaml and reports health.Paused immediately.
	pauseRes, err := rec.Pause(ctx, "sextant")
	if err != nil || !pauseRes.Applied || !pauseRes.Delivered || pauseRes.Report.State != health.Paused {
		t.Fatalf("Pause: res=%+v err=%v", pauseRes, err)
	}
	cur, err = manifest.Parse(pf["sextant"])
	if err != nil || cur.Replicas != 0 || cur.Digest != d3 {
		t.Fatalf("manifest after Pause: %+v err=%v", cur, err)
	}

	// 3. Resume scales replicas to 2 and keeps digest d3.
	if _, err := rec.Resume(ctx, "sextant", 0); !errors.Is(err, errs.ManifestValue) {
		t.Fatalf("Resume(0) expected ManifestValue, got %v", err)
	}
	resRes, err := rec.Resume(ctx, "sextant", 2)
	if err != nil || !resRes.Applied || !resRes.Delivered {
		t.Fatalf("Resume(2): res=%+v err=%v", resRes, err)
	}
	cluster.RollOut("cs-workers", "sextant")
	cur, err = manifest.Parse(pf["sextant"])
	if err != nil || cur.Replicas != 2 || cur.Digest != d3 {
		t.Fatalf("manifest after Resume: %+v err=%v", cur, err)
	}

	// 4. Rollback with empty targetDigest steps back to immediately preceding digest d2.
	rb1, err := rec.Rollback(ctx, "sextant", "")
	if err != nil || !rb1.Applied || !rb1.Delivered {
		t.Fatalf("Rollback previous: res=%+v err=%v", rb1, err)
	}
	cur, err = manifest.Parse(pf["sextant"])
	if err != nil || cur.Digest != d2 || cur.Replicas != 2 {
		t.Fatalf("manifest after Rollback previous: %+v err=%v", cur, err)
	}

	// 5. Rollback with explicit targetDigest d1 pins d1; rolling back again from oldest fails with RollbackNoPrevious.
	rb2, err := rec.Rollback(ctx, "sextant", d1)
	if err != nil || !rb2.Applied {
		t.Fatalf("Rollback explicit d1: res=%+v err=%v", rb2, err)
	}
	if _, err := rec.Rollback(ctx, "sextant", ""); !errors.Is(err, errs.RollbackNoPrevious) {
		t.Fatalf("Rollback at oldest digest expected RollbackNoPrevious, got %v", err)
	}

	// 6. Push d4, d5, d6 so d1 ages out of Lazarette's 5-digest ring; rolling back to d1 must fail with DigestMissing.
	laz.Push("sextant", d4)
	laz.Push("sextant", d5)
	laz.Push("sextant", d6)
	if _, err := rec.Upgrade(ctx, "sextant"); err != nil {
		t.Fatalf("Upgrade to d6: %v", err)
	}
	if _, err := rec.Rollback(ctx, "sextant", d1); !errors.Is(err, errs.DigestMissing) {
		t.Fatalf("Rollback to aged-out d1 expected DigestMissing, got %v", err)
	}

	// 7. An out-of-band SSA field conflict aborts during dry-run BEFORE modifying Plainforge.
	beforeConflict := string(pf["sextant"])
	cluster.Edit("cs-workers", "sextant", "kubectl-edit", "spec.replicas", 5)
	if _, err := rec.Pause(ctx, "sextant"); !errors.Is(err, errs.ApplyConflict) {
		t.Fatalf("Pause under field conflict expected ApplyConflict, got %v", err)
	}
	if string(pf["sextant"]) != beforeConflict {
		t.Fatalf("Plainforge app.yaml mutated despite dry-run ApplyConflict:\ngot:  %s\nwant: %s", pf["sextant"], beforeConflict)
	}
}
