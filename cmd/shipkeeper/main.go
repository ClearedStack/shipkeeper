// Command shipkeeper runs Shipkeeper in three modes:
//
//   - "shipkeeper serve [flags]" runs the in-cluster control-plane service:
//     reconciles all seventeen platform workloads from Plainforge and
//     Lazarette with server-side apply, reports their cleaned contract v1
//     health to Muster, retries delivery across Muster restarts, and answers
//     GET /healthz for kubelet probes.
//
//   - "shipkeeper <upgrade|pause|resume|rollback> -app <name> [flags]" updates
//     app.yaml in Plainforge with [skip-ci], applies the rendered Kubernetes
//     objects with server-side apply, delivers contract v1 health to Muster,
//     and prints the sanitized observation report on standard output.
//
//   - "shipkeeper -app <name> -manifest <file> -digests <file> [flags]" runs
//     one standalone pass for a single app from local files and prints the
//     sanitized observation report on standard output.
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
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/health"
	"github.com/ClearedStack/shipkeeper/internal/kube"
	"github.com/ClearedStack/shipkeeper/internal/lazarette"
	"github.com/ClearedStack/shipkeeper/internal/manifest"
	"github.com/ClearedStack/shipkeeper/internal/muster"
	"github.com/ClearedStack/shipkeeper/internal/plainforge"
	"github.com/ClearedStack/shipkeeper/internal/platform"
	"github.com/ClearedStack/shipkeeper/internal/reconcile"
)

const (
	serviceAccount       = "/var/run/secrets/kubernetes.io/serviceaccount/"
	defaultPlainforgeURL = "http://plainforge.clearedstack.svc.cluster.local:8080"
	defaultLazaretteURL  = "http://lazarette.clearedstack.svc.cluster.local:5000"
	defaultRegistryHost  = "lazarette.clearedstack.svc.cluster.local:5000"
	defaultMusterURL     = "http://muster.clearedstack.svc.cluster.local:8080"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(runContext(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// stdoutMuster stands in for Muster in standalone and CLI lifecycle modes by
// printing the cleaned observation report.
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
	return runContext(context.Background(), args, stdout, stderr)
}

func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			return runServe(ctx, args[1:], stdout, stderr)
		case "upgrade", "pause", "resume", "rollback":
			return runLifecycle(ctx, args[0], args[1:], stdout, stderr)
		}
	}
	return runOnce(ctx, args, stdout, stderr)
}

func runOnce(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shipkeeper", flag.ContinueOnError)
	fs.SetOutput(stderr)
	app := fs.String("app", "", "name of the app to deploy")
	manifestFile := fs.String("manifest", "", "path to the app's app.yaml (stands in for Plainforge)")
	digestsFile := fs.String("digests", "", `path to a JSON file of signed digests, {"image": ["sha256:..."]} (stands in for Lazarette)`)
	kubeURL := fs.String("kube-url", "", "Kubernetes API URL; empty means use the in-cluster service account")
	tokenFile := fs.String("kube-token-file", "", "file holding the bearer token")
	caFile := fs.String("kube-ca-file", "", "file holding the API server's CA certificate")
	registryHost := fs.String("registry", defaultRegistryHost, "registry host images are pulled from")
	domain := fs.String("cluster-domain", "cluster.local", "the cluster's DNS domain")
	timeout := fs.Duration("timeout", 10*time.Second, "time allowed for each request")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	r, err := buildOnce(*app, *manifestFile, *digestsFile, *kubeURL, *tokenFile, *caFile, *timeout)
	if err == nil {
		r.Registry, r.ClusterDomain, r.Muster = *registryHost, *domain, stdoutMuster{stdout}
		_, err = r.Once(ctx, *app)
	}
	if err != nil {
		writeCatalogueError(stderr, err)
		return 1
	}
	return 0
}

func runLifecycle(ctx context.Context, action string, args []string, stdout, stderr io.Writer) int {
	defaultTimeout, timeoutOK := durationEnv("SHIPKEEPER_TIMEOUT", 10*time.Second)

	fs := flag.NewFlagSet("shipkeeper "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	app := fs.String("app", "", "name of the app")
	var replicas *int
	if action == "resume" {
		replicas = fs.Int("replicas", 1, "desired replica count (1..10)")
	}
	var targetDigest *string
	if action == "rollback" {
		targetDigest = fs.String("to", "", "target signed sha256 digest; empty rolls back one step")
	}
	plainforgeURL := fs.String("plainforge-url", envOr("PLAINFORGE_URL", defaultPlainforgeURL), "Plainforge base URL")
	lazaretteURL := fs.String("lazarette-url", envOr("LAZARETTE_URL", defaultLazaretteURL), "Lazarette HTTP API URL")
	cosignPubFile := fs.String("cosign-pub-file", os.Getenv("COSIGN_PUB_FILE"), "file holding the ECDSA P-256 cosign public key PEM")
	registryHost := fs.String("registry", envOr("LAZARETTE_REGISTRY", defaultRegistryHost), "registry host images are pulled from")
	musterURL := fs.String("muster-url", envOr("MUSTER_URL", defaultMusterURL), "Muster HTTP API URL, or off")
	musterTokenFile := fs.String("muster-token-file", os.Getenv("MUSTER_TOKEN_FILE"), "file holding the bearer token presented to Muster")
	kubeURL := fs.String("kube-url", os.Getenv("KUBE_URL"), "Kubernetes API URL; empty means use the in-cluster service account")
	kubeTokenFile := fs.String("kube-token-file", os.Getenv("KUBE_TOKEN_FILE"), "file holding the Kubernetes bearer token")
	kubeCAFile := fs.String("kube-ca-file", os.Getenv("KUBE_CA_FILE"), "file holding the Kubernetes API CA certificate")
	domain := fs.String("cluster-domain", envOr("SHIPKEEPER_CLUSTER_DOMAIN", "cluster.local"), "the cluster's DNS domain")
	timeout := fs.Duration("timeout", defaultTimeout, "time allowed for each HTTP request")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !timeoutOK || *app == "" {
		writeCatalogueError(stderr, errs.Config)
		return 1
	}

	rec, err := buildServe(serveConfig{
		plainforgeURL:   *plainforgeURL,
		lazaretteURL:    *lazaretteURL,
		cosignPubFile:   *cosignPubFile,
		registryHost:    *registryHost,
		musterURL:       *musterURL,
		musterTokenFile: *musterTokenFile,
		kubeURL:         *kubeURL,
		kubeTokenFile:   *kubeTokenFile,
		kubeCAFile:      *kubeCAFile,
		clusterDomain:   *domain,
		interval:        time.Second,
		timeout:         *timeout,
		allowMusterOff:  true,
	})
	if err != nil {
		writeCatalogueError(stderr, err)
		return 1
	}
	rec.Muster = stdoutMuster{stdout}

	switch action {
	case "upgrade":
		_, err = rec.Upgrade(ctx, *app)
	case "pause":
		_, err = rec.Pause(ctx, *app)
	case "resume":
		_, err = rec.Resume(ctx, *app, *replicas)
	case "rollback":
		_, err = rec.Rollback(ctx, *app, *targetDigest)
	}
	if err != nil {
		writeCatalogueError(stderr, err)
		return 1
	}
	return 0
}

func runServe(ctx context.Context, args []string, _, stderr io.Writer) int {
	defaultInterval, intervalOK := durationEnv("SHIPKEEPER_INTERVAL", 30*time.Second)
	defaultTimeout, timeoutOK := durationEnv("SHIPKEEPER_TIMEOUT", 10*time.Second)

	fs := flag.NewFlagSet("shipkeeper serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	plainforgeURL := fs.String("plainforge-url", envOr("PLAINFORGE_URL", defaultPlainforgeURL), "Plainforge base URL")
	lazaretteURL := fs.String("lazarette-url", envOr("LAZARETTE_URL", defaultLazaretteURL), "Lazarette HTTP API URL")
	cosignPubFile := fs.String("cosign-pub-file", os.Getenv("COSIGN_PUB_FILE"), "file holding the ECDSA P-256 cosign public key PEM")
	registryHost := fs.String("registry", envOr("LAZARETTE_REGISTRY", defaultRegistryHost), "registry host images are pulled from")
	musterURL := fs.String("muster-url", envOr("MUSTER_URL", defaultMusterURL), "Muster HTTP API URL")
	musterTokenFile := fs.String("muster-token-file", os.Getenv("MUSTER_TOKEN_FILE"), "file holding the bearer token presented to Muster")
	kubeURL := fs.String("kube-url", os.Getenv("KUBE_URL"), "Kubernetes API URL; empty means use the in-cluster service account")
	kubeTokenFile := fs.String("kube-token-file", os.Getenv("KUBE_TOKEN_FILE"), "file holding the Kubernetes bearer token")
	kubeCAFile := fs.String("kube-ca-file", os.Getenv("KUBE_CA_FILE"), "file holding the Kubernetes API CA certificate")
	domain := fs.String("cluster-domain", envOr("SHIPKEEPER_CLUSTER_DOMAIN", "cluster.local"), "the cluster's DNS domain")
	listenAddr := fs.String("listen", envOr("SHIPKEEPER_LISTEN", ":8080"), "address for GET /healthz, or off")
	interval := fs.Duration("interval", defaultInterval, "time between reconciliation passes")
	timeout := fs.Duration("timeout", defaultTimeout, "time allowed for each HTTP request")
	once := fs.Bool("once", false, "run one full reconciliation pass and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !intervalOK || !timeoutOK {
		writeCatalogueError(stderr, errs.Config)
		return 1
	}

	rec, err := buildServe(serveConfig{
		plainforgeURL:   *plainforgeURL,
		lazaretteURL:    *lazaretteURL,
		cosignPubFile:   *cosignPubFile,
		registryHost:    *registryHost,
		musterURL:       *musterURL,
		musterTokenFile: *musterTokenFile,
		kubeURL:         *kubeURL,
		kubeTokenFile:   *kubeTokenFile,
		kubeCAFile:      *kubeCAFile,
		clusterDomain:   *domain,
		interval:        *interval,
		timeout:         *timeout,
	})
	if err != nil {
		writeCatalogueError(stderr, err)
		return 1
	}

	var srv *http.Server
	if *listenAddr != "" && *listenAddr != "off" {
		ln, err := net.Listen("tcp", *listenAddr)
		if err != nil {
			writeCatalogueError(stderr, errs.Config)
			return 1
		}
		srv = newHealthzServer()
		go func() { _ = srv.Serve(ln) }()
		defer func() {
			shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutCtx)
		}()
	}

	logPass(stderr, rec.SyncAll(ctx))
	if *once {
		return 0
	}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
			logPass(stderr, rec.SyncAll(ctx))
		}
	}
}

type serveConfig struct {
	plainforgeURL   string
	lazaretteURL    string
	cosignPubFile   string
	registryHost    string
	musterURL       string
	musterTokenFile string
	kubeURL         string
	kubeTokenFile   string
	kubeCAFile      string
	clusterDomain   string
	interval        time.Duration
	timeout         time.Duration
	allowMusterOff  bool
}

func buildServe(cfg serveConfig) (*reconcile.Reconciler, error) {
	musterOff := cfg.allowMusterOff && cfg.musterURL == "off"
	if cfg.plainforgeURL == "" || cfg.lazaretteURL == "" || (!musterOff && cfg.musterURL == "") ||
		!manifest.RegistryRE.MatchString(cfg.registryHost) || cfg.interval <= 0 || cfg.timeout <= 0 {
		return nil, errs.Config
	}
	if cfg.cosignPubFile != "" {
		rawKey, err := os.ReadFile(cfg.cosignPubFile)
		if err != nil {
			return nil, errs.Config
		}
		if _, err := lazarette.ParsePublicKey(rawKey); err != nil {
			return nil, err
		}
	}
	kubeClient, resolvedTokenFile, err := buildKubeClient(cfg.kubeURL, cfg.kubeTokenFile, cfg.kubeCAFile, cfg.timeout)
	if err != nil {
		return nil, err
	}
	mTokFile := cfg.musterTokenFile
	if mTokFile == "" {
		mTokFile = resolvedTokenFile
	}
	if !musterOff && mTokFile != "" {
		if raw, err := os.ReadFile(mTokFile); err != nil || strings.TrimSpace(string(raw)) == "" {
			return nil, errs.Config
		}
	}
	sharedHTTP := &http.Client{
		Timeout: cfg.timeout,
		Transport: &http.Transport{
			MaxIdleConns:        4,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     60 * time.Second,
		},
	}
	var contractMuster muster.Sender
	if !musterOff {
		contractMuster = &muster.Client{
			BaseURL:   cfg.musterURL,
			TokenFile: mTokFile,
			HTTP:      sharedHTTP,
		}
	}
	return &reconcile.Reconciler{
		Plainforge: &plainforge.Client{
			BaseURL: cfg.plainforgeURL,
			HTTP:    sharedHTTP,
		},
		Lazarette: &lazarette.Client{
			BaseURL:       cfg.lazaretteURL,
			HTTP:          sharedHTTP,
			PublicKeyFile: cfg.cosignPubFile,
		},
		ContractMuster: contractMuster,
		Cluster:        kubeClient,
		Registry:       cfg.registryHost,
		ClusterDomain:  cfg.clusterDomain,
	}, nil
}

func buildOnce(app, manifestFile, digestsFile, kubeURL, tokenFile, caFile string, timeout time.Duration) (*reconcile.Reconciler, error) {
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
	laz := &platform.MemoryLazarette{}
	for image, list := range digests {
		for _, d := range list {
			laz.Push(image, d)
		}
	}
	kubeClient, _, err := buildKubeClient(kubeURL, tokenFile, caFile, timeout)
	if err != nil {
		return nil, err
	}
	return &reconcile.Reconciler{
		Plainforge: platform.MemoryPlainforge{app: data},
		Lazarette:  laz,
		Cluster:    kubeClient,
	}, nil
}

func buildKubeClient(kubeURL, tokenFile, caFile string, timeout time.Duration) (*kube.Client, string, error) {
	if kubeURL == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" || port == "" {
			return nil, "", errs.Config
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
	transport := &http.Transport{
		MaxIdleConns:        4,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     60 * time.Second,
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		pool := x509.NewCertPool()
		if err != nil || !pool.AppendCertsFromPEM(pem) {
			return nil, "", errs.Config
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	if tokenFile != "" {
		raw, err := os.ReadFile(tokenFile)
		if err != nil || strings.TrimSpace(string(raw)) == "" {
			return nil, "", errs.Config
		}
	}
	return &kube.Client{
		BaseURL:      strings.TrimRight(kubeURL, "/"),
		TokenFile:    tokenFile,
		FieldManager: manifest.FieldManager,
		HTTP:         &http.Client{Timeout: timeout, Transport: transport},
	}, tokenFile, nil
}

func newHealthzServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
}

func logPass(stderr io.Writer, sum reconcile.SyncSummary) {
	slugs := make([]string, 0, len(sum.ComponentErrors))
	for slug := range sum.ComponentErrors {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	for _, slug := range slugs {
		err := sum.ComponentErrors[slug]
		fmt.Fprintf(stderr, "%s: %s\n", slug, errs.Public(err))
		var e *errs.Error
		if errors.As(err, &e) && len(e.Fields) > 0 {
			fmt.Fprintf(stderr, "%s: conflicting fields: %s\n", slug, strings.Join(e.Fields, ", "))
		}
	}
	if sum.MusterError != nil {
		writeCatalogueError(stderr, sum.MusterError)
	}
}

func writeCatalogueError(stderr io.Writer, err error) {
	fmt.Fprintln(stderr, errs.Public(err))
	var e *errs.Error
	if errors.As(err, &e) && len(e.Fields) > 0 {
		fmt.Fprintln(stderr, "conflicting fields: "+strings.Join(e.Fields, ", "))
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) (time.Duration, bool) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, true
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}
