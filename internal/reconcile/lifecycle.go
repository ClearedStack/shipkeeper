package reconcile

import (
	"context"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/health"
	"github.com/ClearedStack/shipkeeper/internal/manifest"
	"github.com/ClearedStack/shipkeeper/internal/muster"
	"github.com/ClearedStack/shipkeeper/internal/plainforge"
	"github.com/ClearedStack/shipkeeper/internal/platform"
	"github.com/ClearedStack/shipkeeper/internal/registry"
)

// Upgrade pins appName's app.yaml in Plainforge to the newest retained signed
// digest held by Lazarette (marked [skip-ci]), applies the rendered objects
// with server-side apply, observes rollout state, and delivers the cleaned
// health report to Muster.
func (r *Reconciler) Upgrade(ctx context.Context, appName string) (Result, error) {
	if _, ok := r.Plainforge.(platform.PlainforgeWriter); !ok {
		return Result{}, errs.Config
	}
	retLaz, ok := r.Lazarette.(platform.RetainedLazarette)
	if !ok {
		return Result{}, errs.Config
	}

	data, curApp, err := r.readCurrentManifest(ctx, appName)
	if err != nil {
		return Result{}, err
	}
	digests, err := retLaz.RetainedDigests(ctx, curApp.Image)
	if err != nil {
		return Result{}, err
	}
	if len(digests) == 0 {
		return Result{}, errs.DigestMissing
	}
	latest := digests[len(digests)-1]
	signed, err := r.Lazarette.HasSignedDigest(ctx, curApp.Image, latest)
	if err != nil {
		return Result{}, err
	}
	if !signed {
		return Result{}, errs.DigestMissing
	}

	updatedYAML, changed, err := plainforge.UpdateAppYAML(data, latest, nil)
	if err != nil {
		return Result{}, err
	}
	return r.commitAndApply(ctx, appName, updatedYAML, changed, "shipkeeper: upgrade "+appName+" [skip-ci]")
}

// Pause sets replicas: 0 in appName's app.yaml in Plainforge (marked
// [skip-ci]) and applies the Deployment with zero replicas while keeping its
// Service, NetworkPolicy, configuration, PVCs, and queue state intact.
func (r *Reconciler) Pause(ctx context.Context, appName string) (Result, error) {
	if _, ok := r.Plainforge.(platform.PlainforgeWriter); !ok {
		return Result{}, errs.Config
	}
	data, curApp, err := r.readCurrentManifest(ctx, appName)
	if err != nil {
		return Result{}, err
	}
	signed, err := r.Lazarette.HasSignedDigest(ctx, curApp.Image, curApp.Digest)
	if err != nil {
		return Result{}, err
	}
	if !signed {
		return Result{}, errs.DigestMissing
	}

	zero := 0
	updatedYAML, changed, err := plainforge.UpdateAppYAML(data, "", &zero)
	if err != nil {
		return Result{}, err
	}
	return r.commitAndApply(ctx, appName, updatedYAML, changed, "shipkeeper: pause "+appName+" [skip-ci]")
}

// Resume restores replicas (1..10) in appName's app.yaml in Plainforge (marked
// [skip-ci]), applies the updated objects with server-side apply, and reports
// health to Muster.
func (r *Reconciler) Resume(ctx context.Context, appName string, replicas int) (Result, error) {
	if _, ok := r.Plainforge.(platform.PlainforgeWriter); !ok {
		return Result{}, errs.Config
	}
	if replicas < 1 || replicas > 10 {
		return Result{}, errs.ManifestValue
	}
	data, curApp, err := r.readCurrentManifest(ctx, appName)
	if err != nil {
		return Result{}, err
	}
	signed, err := r.Lazarette.HasSignedDigest(ctx, curApp.Image, curApp.Digest)
	if err != nil {
		return Result{}, err
	}
	if !signed {
		return Result{}, errs.DigestMissing
	}

	updatedYAML, changed, err := plainforge.UpdateAppYAML(data, "", &replicas)
	if err != nil {
		return Result{}, err
	}
	return r.commitAndApply(ctx, appName, updatedYAML, changed, "shipkeeper: resume "+appName+" [skip-ci]")
}

// Rollback pins appName's app.yaml in Plainforge to either targetDigest (one
// of the retained signed digests in Lazarette) or, when targetDigest is empty,
// the immediately preceding retained signed digest before the current one.
func (r *Reconciler) Rollback(ctx context.Context, appName, targetDigest string) (Result, error) {
	if _, ok := r.Plainforge.(platform.PlainforgeWriter); !ok {
		return Result{}, errs.Config
	}
	retLaz, ok := r.Lazarette.(platform.RetainedLazarette)
	if !ok {
		return Result{}, errs.Config
	}
	if targetDigest != "" && !pinnedDigestRE.MatchString(targetDigest) {
		return Result{}, errs.ManifestValue
	}

	data, curApp, err := r.readCurrentManifest(ctx, appName)
	if err != nil {
		return Result{}, err
	}
	digests, err := retLaz.RetainedDigests(ctx, curApp.Image)
	if err != nil {
		return Result{}, err
	}

	var chosen string
	if targetDigest == "" {
		idx := -1
		for i := len(digests) - 1; i >= 0; i-- {
			if digests[i] == curApp.Digest {
				idx = i
				break
			}
		}
		if idx <= 0 {
			return Result{}, errs.RollbackNoPrevious
		}
		chosen = digests[idx-1]
	} else {
		found := false
		for _, d := range digests {
			if d == targetDigest {
				found = true
				break
			}
		}
		if !found {
			return Result{}, errs.DigestMissing
		}
		chosen = targetDigest
	}

	signed, err := r.Lazarette.HasSignedDigest(ctx, curApp.Image, chosen)
	if err != nil {
		return Result{}, err
	}
	if !signed {
		return Result{}, errs.DigestMissing
	}

	updatedYAML, changed, err := plainforge.UpdateAppYAML(data, chosen, nil)
	if err != nil {
		return Result{}, err
	}
	return r.commitAndApply(ctx, appName, updatedYAML, changed, "shipkeeper: rollback "+appName+" [skip-ci]")
}

func (r *Reconciler) readCurrentManifest(ctx context.Context, appName string) ([]byte, manifest.App, error) {
	if r.Plainforge == nil || r.Lazarette == nil || r.Cluster == nil {
		return nil, manifest.App{}, errs.Config
	}
	data, err := r.Plainforge.AppManifest(ctx, appName)
	if err != nil {
		return nil, manifest.App{}, err
	}
	app, err := manifest.Parse(data)
	if err != nil {
		return nil, manifest.App{}, err
	}
	if app.Name != appName {
		return nil, manifest.App{}, errs.ManifestOtherApp
	}
	if comp, ok := registry.Lookup(appName); ok && app.Namespace != comp.Namespace {
		return nil, manifest.App{}, errs.ManifestNamespace
	}
	return data, app, nil
}

func (r *Reconciler) commitAndApply(ctx context.Context, appName string, updatedYAML []byte, changed bool, summary string) (Result, error) {
	var res Result
	writer, ok := r.Plainforge.(platform.PlainforgeWriter)
	if !ok {
		return res, errs.Config
	}

	nextApp, err := manifest.Parse(updatedYAML)
	if err != nil {
		return res, err
	}
	objects, err := manifest.Render(nextApp, r.Registry)
	if err != nil {
		return res, err
	}

	// Dry-run server-side apply first so an ownership conflict aborts before
	// writing to Plainforge or mutating the cluster.
	for _, o := range objects {
		if err := r.Cluster.Apply(ctx, o, true); err != nil {
			return res, err
		}
	}

	if changed {
		if err := writer.UpdateAppManifest(ctx, appName, updatedYAML, summary); err != nil {
			return res, err
		}
	}

	for _, o := range objects {
		if err := r.Cluster.Apply(ctx, o, false); err != nil {
			return res, err
		}
	}
	res.Applied = true

	obs, err := r.Cluster.Observe(ctx, nextApp.Namespace, nextApp.Name, manifest.SelectorLabel)
	if err != nil {
		return res, err
	}
	report, err := health.Build(nextApp.Name, nextApp.Namespace, obs, health.Config{
		ClusterDomain: r.ClusterDomain,
		Namespaces:    manifest.Namespaces,
	})
	if err != nil {
		return res, err
	}
	res.Report = &report

	if r.ContractMuster != nil {
		if _, err := r.ContractMuster.SendHealth(ctx, []muster.Report{toMusterReport(nextApp.Name, report.State, nextApp.Digest)}); err != nil {
			return res, err
		}
	}
	if r.Muster != nil {
		if err := r.Muster.SendHealth(ctx, report); err != nil {
			return res, err
		}
	}
	if r.Muster != nil || r.ContractMuster != nil {
		res.Delivered = true
	}
	return res, nil
}
