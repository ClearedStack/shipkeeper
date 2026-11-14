package reconcile

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/health"
	"github.com/ClearedStack/shipkeeper/internal/kube"
	"github.com/ClearedStack/shipkeeper/internal/manifest"
	"github.com/ClearedStack/shipkeeper/internal/muster"
	"github.com/ClearedStack/shipkeeper/internal/registry"
)

var pinnedDigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// SyncSummary describes one full reconciliation pass across the platform's
// registered components.
type SyncSummary struct {
	Applied         int
	Reports         []muster.Report
	Delivered       bool
	Muster          muster.Summary
	ComponentErrors map[string]error // component slug -> catalogue error
	MusterError     error            // non-nil when POST /v1/health failed
}

// PendingCount returns how many component health reports are buffered in
// memory waiting for Muster or Logbook to recover.
func (r *Reconciler) PendingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

// SyncAll reconciles every registered workload across clearedstack and
// cs-workers and delivers one batch of cleaned contract v1 health reports to
// Muster. If Muster or Logbook is temporarily unreachable or slow, Kubernetes
// reconciliation still completes and the latest report per component is kept
// in memory to retry on the next pass.
func (r *Reconciler) SyncAll(ctx context.Context) SyncSummary {
	comps := r.Components
	if len(comps) == 0 {
		comps = registry.All()
	}
	sum := SyncSummary{
		ComponentErrors: map[string]error{},
	}

	fresh := make([]muster.Report, 0, len(comps))
	for _, comp := range comps {
		rep, applied, ok, err := r.syncOne(ctx, comp)
		if applied {
			sum.Applied++
		}
		if err != nil {
			sum.ComponentErrors[comp.Slug] = err
		}
		if ok {
			fresh = append(fresh, rep)
		}
	}

	batch := r.stagePending(comps, fresh)
	sum.Reports = batch
	if len(batch) == 0 || r.ContractMuster == nil {
		return sum
	}

	ack, err := r.ContractMuster.SendHealth(ctx, batch)
	if err != nil {
		sum.MusterError = err
		if !muster.Retryable(err) {
			r.clearPending()
		}
		return sum
	}
	r.clearPending()
	sum.Muster = ack
	sum.Delivered = true
	return sum
}

func (r *Reconciler) stagePending(order []registry.Component, fresh []muster.Report) []muster.Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		r.pending = make(map[string]muster.Report, len(order))
	}
	for _, rep := range fresh {
		r.pending[rep.Component] = rep
	}
	batch := make([]muster.Report, 0, len(r.pending))
	seen := make(map[string]bool, len(r.pending))
	for _, comp := range order {
		if rep, ok := r.pending[comp.Slug]; ok {
			batch = append(batch, rep)
			seen[comp.Slug] = true
		}
	}
	for slug, rep := range r.pending {
		if !seen[slug] {
			batch = append(batch, rep)
		}
	}
	if len(batch) > muster.MaxReports {
		batch = batch[:muster.MaxReports]
	}
	return batch
}

func (r *Reconciler) clearPending() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.pending)
}

func (r *Reconciler) syncOne(ctx context.Context, comp registry.Component) (rep muster.Report, applied, ok bool, err error) {
	data, err := r.Plainforge.AppManifest(ctx, comp.Slug)
	if errors.Is(err, errs.PlainforgeNoApp) {
		rep, ok, obsErr := r.observeUnmanaged(ctx, comp)
		return rep, false, ok, obsErr
	}
	if err != nil {
		return degradedWithCode(comp.Slug, err), false, true, err
	}

	app, err := manifest.Parse(data)
	if err != nil {
		return degradedWithCode(comp.Slug, err), false, true, err
	}
	if app.Name != comp.Slug {
		return degradedWithCode(comp.Slug, errs.ManifestOtherApp), false, true, errs.ManifestOtherApp
	}
	if app.Namespace != comp.Namespace {
		return degradedWithCode(comp.Slug, errs.ManifestNamespace), false, true, errs.ManifestNamespace
	}

	signed, err := r.Lazarette.HasSignedDigest(ctx, app.Image, app.Digest)
	if err != nil {
		return degradedWithCode(comp.Slug, err), false, true, err
	}
	if !signed {
		return degradedWithCode(comp.Slug, errs.DigestMissing), false, true, errs.DigestMissing
	}

	objects, err := manifest.Render(app, r.Registry)
	if err != nil {
		return degradedWithCode(comp.Slug, err), false, true, err
	}
	for _, dryRun := range []bool{true, false} {
		for _, o := range objects {
			if err := r.Cluster.Apply(ctx, o, dryRun); err != nil {
				return degradedWithCode(comp.Slug, err), false, true, err
			}
		}
	}

	obs, err := r.Cluster.Observe(ctx, app.Namespace, app.Name, manifest.SelectorLabel)
	if err != nil {
		return degradedWithCode(comp.Slug, err), true, true, err
	}
	hr, err := health.Build(app.Name, app.Namespace, obs, health.Config{
		ClusterDomain: r.ClusterDomain,
		Namespaces:    manifest.Namespaces,
	})
	if err != nil {
		return degradedWithCode(comp.Slug, err), true, true, err
	}
	return toMusterReport(comp.Slug, hr.State, app.Digest), true, true, nil
}

func (r *Reconciler) observeUnmanaged(ctx context.Context, comp registry.Component) (muster.Report, bool, error) {
	obsCluster, canObserve := r.Cluster.(ExistingObserver)
	if !canObserve {
		return muster.Report{}, false, nil
	}
	obs, hasDep, err := obsCluster.ObserveExisting(ctx, comp.Namespace, comp.Slug, manifest.SelectorLabel)
	if err != nil {
		return degradedWithCode(comp.Slug, err), true, err
	}
	if hasDep {
		hr, err := health.Build(comp.Slug, comp.Namespace, obs, health.Config{
			ClusterDomain: r.ClusterDomain,
			Namespaces:    manifest.Namespaces,
		})
		if err != nil {
			return degradedWithCode(comp.Slug, err), true, err
		}
		digest := extractSpecsDigest(obs.Deployment.Spec.Template.Spec.Containers, comp.Slug)
		return toMusterReport(comp.Slug, hr.State, digest), true, nil
	}
	if len(obs.Pods) == 0 {
		return muster.Report{}, false, nil
	}
	if podsAllReady(obs.Pods) {
		var digest string
		if len(obs.Pods) > 0 {
			digest = extractSpecsDigest(obs.Pods[0].Spec.Containers, comp.Slug)
		}
		return muster.Report{Component: comp.Slug, State: muster.Healthy, Digest: digest}, true, nil
	}
	return muster.Report{Component: comp.Slug, State: muster.Degraded}, true, nil
}

func podsAllReady(pods []kube.Pod) bool {
	if len(pods) == 0 {
		return false
	}
	for _, p := range pods {
		if p.Status.Phase != "Running" {
			return false
		}
		ready := false
		for _, cond := range p.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == "True" {
				ready = true
				break
			}
		}
		if !ready {
			return false
		}
	}
	return true
}

func extractSpecsDigest(containers []kube.ContainerSpec, preferred string) string {
	for _, c := range containers {
		if c.Name == preferred {
			if d := imageDigest(c.Image); d != "" {
				return d
			}
		}
	}
	if len(containers) == 1 {
		return imageDigest(containers[0].Image)
	}
	return ""
}

func imageDigest(image string) string {
	_, d, ok := strings.Cut(image, "@")
	if ok && pinnedDigestRE.MatchString(d) {
		return d
	}
	return ""
}

func toMusterReport(slug string, st health.State, healthyDigest string) muster.Report {
	switch st {
	case health.Healthy:
		return muster.Report{Component: slug, State: muster.Healthy, Digest: healthyDigest}
	case health.Paused:
		return muster.Report{Component: slug, State: muster.Paused}
	default:
		return muster.Report{Component: slug, State: muster.Degraded}
	}
}

func degradedWithCode(slug string, err error) muster.Report {
	return muster.Report{
		Component: slug,
		State:     muster.Degraded,
		Code:      errs.CodeOf(err),
	}
}
