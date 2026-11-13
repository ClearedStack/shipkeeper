// Package reconcile is the first milestone end to end: read one app.yaml,
// apply it with server-side apply, and report the app's health with cluster
// details removed.
package reconcile

import (
	"context"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/health"
	"github.com/ClearedStack/shipkeeper/internal/kube"
	"github.com/ClearedStack/shipkeeper/internal/manifest"
	"github.com/ClearedStack/shipkeeper/internal/platform"
)

// Cluster is the part of the Kubernetes client the reconciler uses.
type Cluster interface {
	Apply(ctx context.Context, obj kube.Object, dryRun bool) error
	Observe(ctx context.Context, namespace, name, labelKey string) (kube.Observed, error)
}

// Reconciler wires Shipkeeper to its neighbours.
type Reconciler struct {
	Plainforge    platform.Plainforge
	Lazarette     platform.Lazarette
	Muster        platform.Muster
	Cluster       Cluster
	Registry      string // host[:port] images are pulled from
	ClusterDomain string // e.g. cluster.local
}

// Result says how far one pass got.
type Result struct {
	Applied   bool           // the three objects were applied
	Report    *health.Report // the cleaned report, once it was built
	Delivered bool           // Muster accepted the report
}

// Once runs one pass for the named app. It stops at the first thing that is
// not as expected and returns an error from the catalogue:
//
//  1. Read app.yaml from Plainforge and parse it.
//  2. Check its digest is a signed digest held by Lazarette. If not, stop:
//     nothing is sent to the cluster, so what is running keeps running.
//  3. Dry-run the apply of all three objects. If any is refused (a field
//     conflict, for one), stop: nothing has been changed.
//  4. Apply the three objects. Shipkeeper never forces an apply.
//  5. Read the Deployment, its pods and the namespace's events.
//  6. Build the cleaned report and send it to Muster.
//
// A failure in step 5 or 6 does not undo step 4.
func (r *Reconciler) Once(ctx context.Context, appName string) (Result, error) {
	var res Result
	data, err := r.Plainforge.AppManifest(ctx, appName)
	if err != nil {
		return res, err
	}
	app, err := manifest.Parse(data)
	if err != nil {
		return res, err
	}
	if app.Name != appName {
		return res, errs.ManifestOtherApp
	}
	signed, err := r.Lazarette.HasSignedDigest(ctx, app.Image, app.Digest)
	if err != nil {
		return res, err
	}
	if !signed {
		return res, errs.DigestMissing
	}
	objects, err := manifest.Render(app, r.Registry)
	if err != nil {
		return res, err
	}
	for _, dryRun := range []bool{true, false} {
		for _, o := range objects {
			if err := r.Cluster.Apply(ctx, o, dryRun); err != nil {
				return res, err
			}
		}
	}
	res.Applied = true

	obs, err := r.Cluster.Observe(ctx, app.Namespace, app.Name, manifest.SelectorLabel)
	if err != nil {
		return res, err
	}
	report, err := health.Build(app.Name, app.Namespace, obs,
		health.Config{ClusterDomain: r.ClusterDomain, Namespaces: manifest.Namespaces})
	if err != nil {
		return res, err
	}
	res.Report = &report
	if err := r.Muster.SendHealth(ctx, report); err != nil {
		return res, err
	}
	res.Delivered = true
	return res, nil
}
