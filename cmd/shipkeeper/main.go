// Command shipkeeper runs one pass of the first milestone: read one
// app.yaml, apply it to a cluster with server-side apply, and print the
// app's health report with cluster details removed.
//
// Plainforge, Lazarette and Muster do not exist yet. Here app.yaml is read
// from a file, the signed digests from a JSON file, and the report that
// would go to Muster is written to standard output.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/health"
	"github.com/ClearedStack/shipkeeper/internal/kube"
	"github.com/ClearedStack/shipkeeper/internal/manifest"
	"github.com/ClearedStack/shipkeeper/internal/platform"
	"github.com/ClearedStack/shipkeeper/internal/reconcile"
)

const serviceAccount = "/var/run/secrets/kubernetes.io/serviceaccount/"

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// stdoutMuster stands in for Muster by printing the report.
type stdoutMuster struct{ w io.Writer }

func (m stdoutMuster) SendHealth(_ context.Context, r health.Report) error {
	out, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return errs.Unexpected
	}
	_, err = fmt.Fprintf(m.w, "%s\n", out)
	return err
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shipkeeper", flag.ContinueOnError)
	fs.SetOutput(stderr)
	app := fs.String("app", "", "name of the app to deploy")
	manifestFile := fs.String("manifest", "", "path to the app's app.yaml (stands in for Plainforge)")
	digestsFile := fs.String("digests", "", `path to a JSON file of signed digests, {"image": ["sha256:..."]} (stands in for Lazarette)`)
	kubeURL := fs.String("kube-url", "", "Kubernetes API URL; empty means use the in-cluster service account")
	tokenFile := fs.String("kube-token-file", "", "file holding the bearer token")
	caFile := fs.String("kube-ca-file", "", "file holding the API server's CA certificate")
	registry := fs.String("registry", "lazarette.clearedstack.svc.cluster.local:5000", "registry host images are pulled from")
	domain := fs.String("cluster-domain", "cluster.local", "the cluster's DNS domain")
	timeout := fs.Duration("timeout", 10*time.Second, "time allowed for each request")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	r, err := build(*app, *manifestFile, *digestsFile, *kubeURL, *tokenFile, *caFile, *timeout)
	if err == nil {
		r.Registry, r.ClusterDomain, r.Muster = *registry, *domain, stdoutMuster{stdout}
		_, err = r.Once(context.Background(), *app)
	}
	if err != nil {
		fmt.Fprintln(stderr, errs.Public(err))
		var e *errs.Error
		if errors.As(err, &e) && len(e.Fields) > 0 {
			fmt.Fprintln(stderr, "conflicting fields: "+strings.Join(e.Fields, ", "))
		}
		return 1
	}
	return 0
}

func build(app, manifestFile, digestsFile, kubeURL, tokenFile, caFile string, timeout time.Duration) (*reconcile.Reconciler, error) {
	if app == "" || manifestFile == "" || digestsFile == "" || timeout <= 0 {
		return nil, errs.Config
	}
	data, err := os.ReadFile(manifestFile)
	if err != nil {
		return nil, errs.Config
	}
	raw, err := os.ReadFile(digestsFile)
	if err != nil {
		return nil, errs.Config
	}
	var digests map[string][]string
	if json.Unmarshal(raw, &digests) != nil {
		return nil, errs.Config
	}
	lazarette := &platform.MemoryLazarette{}
	for image, list := range digests {
		for _, d := range list {
			lazarette.Push(image, d)
		}
	}

	if kubeURL == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" || port == "" {
			return nil, errs.Config
		}
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		kubeURL = "https://" + host + ":" + port
		if tokenFile == "" {
			tokenFile = serviceAccount + "token"
		}
		if caFile == "" {
			caFile = serviceAccount + "ca.crt"
		}
	}
	client := &kube.Client{
		BaseURL: strings.TrimRight(kubeURL, "/"), FieldManager: manifest.FieldManager,
		HTTP: &http.Client{Timeout: timeout},
	}
	if tokenFile != "" {
		token, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, errs.Config
		}
		client.Token = strings.TrimSpace(string(token))
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		pool := x509.NewCertPool()
		if err != nil || !pool.AppendCertsFromPEM(pem) {
			return nil, errs.Config
		}
		client.HTTP.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	}
	return &reconcile.Reconciler{
		Plainforge: platform.MemoryPlainforge{app: data},
		Lazarette:  lazarette,
		Cluster:    client,
	}, nil
}
