// Command shipkeeper-demo runs the first milestone against the in-memory
// test cluster, so it can be tried with no Kubernetes cluster at hand.
//
// It applies the example app, plays the part of the cluster by adding two
// made-up pods (one of them crash-looping) with node names and addresses,
// runs a second pass, and prints the cleaned health report from that pass.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/example"
	"github.com/ClearedStack/shipkeeper/internal/kube"
	"github.com/ClearedStack/shipkeeper/internal/kubetest"
	"github.com/ClearedStack/shipkeeper/internal/manifest"
	"github.com/ClearedStack/shipkeeper/internal/platform"
	"github.com/ClearedStack/shipkeeper/internal/reconcile"
)

func main() {
	cluster := kubetest.New(manifest.Namespaces...)
	defer cluster.Close()

	lazarette := &platform.MemoryLazarette{}
	for image, digests := range example.Digests() {
		for _, d := range digests {
			lazarette.Push(image, d)
		}
	}
	muster := &platform.MemoryMuster{}
	r := &reconcile.Reconciler{
		Plainforge: platform.MemoryPlainforge{example.App: example.File("app.yaml")},
		Lazarette:  lazarette,
		Muster:     muster,
		Cluster: &kube.Client{BaseURL: cluster.URL, FieldManager: manifest.FieldManager,
			HTTP: &http.Client{Timeout: 5 * time.Second}},
		Registry:      "lazarette.clearedstack.svc.cluster.local:5000",
		ClusterDomain: "cluster.local",
	}

	for pass := 1; pass <= 2; pass++ {
		if _, err := r.Once(context.Background(), example.App); err != nil {
			fmt.Fprintln(os.Stderr, errs.Public(err))
			os.Exit(1)
		}
		if pass == 1 {
			for _, p := range example.Objects("pods.json") {
				cluster.AddPod(example.Namespace, p)
			}
			for _, e := range example.Objects("events.json") {
				cluster.AddEvent(example.Namespace, e)
			}
			cluster.SetDeploymentStatus(example.Namespace, example.App, map[string]any{
				"observedGeneration": 1, "replicas": 2, "updatedReplicas": 2, "readyReplicas": 1, "availableReplicas": 1,
			})
		}
	}
	out, _ := json.MarshalIndent(muster.Reports[len(muster.Reports)-1], "", "  ")
	fmt.Printf("%s\n", out)
}
