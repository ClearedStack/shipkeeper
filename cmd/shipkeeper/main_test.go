package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ClearedStack/shipkeeper/internal/example"
	"github.com/ClearedStack/shipkeeper/internal/golden"
	"github.com/ClearedStack/shipkeeper/internal/kubetest"
	"github.com/ClearedStack/shipkeeper/internal/muster"
)

func files(t *testing.T) (manifest, digests string) {
	t.Helper()
	dir := t.TempDir()
	manifest, digests = filepath.Join(dir, "app.yaml"), filepath.Join(dir, "digests.json")
	os.WriteFile(manifest, example.File("app.yaml"), 0o600)
	os.WriteFile(digests, example.File("digests.json"), 0o600)
	return
}

func TestRunPrintsTheReport(t *testing.T) {
	srv := kubetest.New("clearedstack", "cs-workers")
	defer srv.Close()
	manifest, digests := files(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-app", "example-app", "-manifest", manifest, "-digests", digests, "-kube-url", srv.URL}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	golden.Equal(t, "testdata/stdout.json", stdout.Bytes())
}

func TestRunPrintsOnlyCatalogueErrors(t *testing.T) {
	srv := kubetest.New("clearedstack", "cs-workers")
	manifest, digests := files(t)
	args := []string{"-app", "example-app", "-manifest", manifest, "-digests", digests, "-kube-url", srv.URL}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("first pass: exit %d, %s", code, stderr.String())
	}

	srv.Edit("cs-workers", "example-app", "someone-with-kubectl", "spec.replicas", 9)
	stdout.Reset()
	if code := run(args, &stdout, &stderr); code != 1 || stdout.Len() != 0 {
		t.Fatalf("conflict: exit %d, stdout %q", code, stdout.String())
	}
	golden.Equal(t, "testdata/stderr-conflict.txt", stderr.Bytes())

	srv.Close()
	stderr.Reset()
	if code := run(args, &stdout, &stderr); code != 1 {
		t.Fatalf("cluster gone: exit %d", code)
	}
	golden.Equal(t, "testdata/stderr-unreachable.txt", stderr.Bytes())

	stderr.Reset()
	if code := run([]string{"-app", "example-app"}, &stdout, &stderr); code != 1 {
		t.Fatalf("missing settings: exit %d", code)
	}
	golden.Equal(t, "testdata/stderr-config.txt", stderr.Bytes())
}

func TestServeOnceEndToEnd(t *testing.T) {
	const backend = "/usr/lib/git-core/git-http-backend"
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend not installed")
	}

	const assayDigest = "sha256:abababababababababababababababababababababababababababababababab"
	gitRoot := t.TempDir()
	assayBare := filepath.Join(gitRoot, "assay.git")
	gitCmd := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Shipkeeper",
			"GIT_AUTHOR_EMAIL=test@example.invalid",
			"GIT_COMMITTER_NAME=Shipkeeper",
			"GIT_COMMITTER_EMAIL=test@example.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitCmd(gitRoot, "init", "--bare", "--initial-branch=main", assayBare)
	gitCmd(assayBare, "config", "http.uploadpack", "true")

	work := filepath.Join(gitRoot, "work")
	gitCmd(gitRoot, "init", "--initial-branch=main", work)
	appYAML := "name: assay\nnamespace: cs-workers\nimage: assay\ndigest: " + assayDigest + "\nreplicas: 1\nport: 8080\nmemory: 32Mi\nallowFrom:\n  - clearedstack/muster\n"
	if err := os.WriteFile(filepath.Join(work, "app.yaml"), []byte(appYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(work, "add", ".")
	gitCmd(work, "commit", "-m", "assay manifest")
	gitCmd(work, "push", assayBare, "main:main")

	plainforgeSrv := httptest.NewServer(&cgi.Handler{
		Path:   backend,
		Stderr: io.Discard,
		Env: []string{
			"GIT_PROJECT_ROOT=" + gitRoot,
			"GIT_HTTP_EXPORT_ALL=1",
		},
	})
	defer plainforgeSrv.Close()

	lazaretteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == "/v2/assay/manifests/"+assayDigest {
			w.Header().Set("Docker-Content-Digest", assayDigest)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer lazaretteSrv.Close()

	memMuster := &muster.Memory{
		Tokens:         map[string]string{"shipkeeper-token": "shipkeeper"},
		Digests:        map[string]string{"assay": "sha256:1010101010101010101010101010101010101010101010101010101010101010"},
		PausedBy:       map[string]int{"assay": 2},
		BuildingByRepo: map[string]int{"assay": 1},
	}
	musterSrv := httptest.NewServer(memMuster)
	defer musterSrv.Close()

	cluster := kubetest.New("clearedstack", "cs-workers")
	defer cluster.Close()
	cluster.Token = "shipkeeper-token"

	tokFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokFile, []byte("shipkeeper-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	args := []string{
		"serve",
		"-once",
		"-listen", "127.0.0.1:0",
		"-plainforge-url", plainforgeSrv.URL,
		"-lazarette-url", lazaretteSrv.URL,
		"-muster-url", musterSrv.URL,
		"-kube-url", cluster.URL,
		"-kube-token-file", tokFile,
	}
	if code := runContext(context.Background(), args, &stdout, &stderr); code != 0 {
		t.Fatalf("first serve -once: exit %d, stderr %q", code, stderr.String())
	}
	if cluster.Field("cs-workers", "assay", "spec.replicas") != 1.0 {
		t.Fatalf("assay deployment was not applied to cs-workers")
	}

	// Mark assay rolled out in the cluster and run a second pass: its digest is
	// now healthy, so Muster replays paused items and marks the proposal live.
	cluster.RollOut("cs-workers", "assay")
	stderr.Reset()
	if code := runContext(context.Background(), args, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("second serve -once: exit %d, stderr %q", code, stderr.String())
	}
	last := memMuster.LastReports()
	if len(last) != 1 || last[0].Component != "assay" || last[0].State != muster.Healthy || last[0].Digest != assayDigest {
		t.Fatalf("muster last batch: %+v", last)
	}
	if got := memMuster.PausedBy["assay"]; got != 0 {
		t.Fatalf("paused items remaining: got %d, want 0", got)
	}
	if got := memMuster.BuildingByRepo["assay"]; got != 0 {
		t.Fatalf("building proposals remaining: got %d, want 0", got)
	}
}

func TestHealthzServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newHealthzServer()
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "{\"status\":\"ok\"}\n" {
		t.Fatalf("GET /healthz: status=%d body=%q", resp.StatusCode, body)
	}
}
