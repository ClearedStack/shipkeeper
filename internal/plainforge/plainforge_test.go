package plainforge_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/plainforge"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Shipkeeper Test",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Shipkeeper Test",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestAppManifestOverGitSmartHTTP(t *testing.T) {
	const backend = "/usr/lib/git-core/git-http-backend"
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend not installed")
	}

	root := t.TempDir()
	sextantBare := filepath.Join(root, "sextant.git")
	runGit(t, root, "init", "--bare", "--initial-branch=main", sextantBare)
	runGit(t, sextantBare, "config", "http.uploadpack", "true")

	connBare := filepath.Join(root, "clearedstack.com.git")
	runGit(t, root, "init", "--bare", "--initial-branch=main", connBare)
	runGit(t, connBare, "config", "http.uploadpack", "true")

	emptyBare := filepath.Join(root, "trawl.git")
	runGit(t, root, "init", "--bare", "--initial-branch=main", emptyBare)
	runGit(t, emptyBare, "config", "http.uploadpack", "true")

	work := filepath.Join(root, "work")
	runGit(t, root, "init", "--initial-branch=main", work)
	firstYAML := "name: sextant\nnamespace: cs-workers\nimage: sextant\ndigest: sha256:1111111111111111111111111111111111111111111111111111111111111111\nreplicas: 1\nport: 8080\nmemory: 30Mi\n"
	if err := os.WriteFile(filepath.Join(work, "app.yaml"), []byte(firstYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# sextant\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "initial manifest")
	runGit(t, work, "push", sextantBare, "main:main")

	// Also push conn's manifest to clearedstack.com.git (testing registry repo mapping).
	connWork := filepath.Join(root, "conn-work")
	runGit(t, root, "init", "--initial-branch=main", connWork)
	connYAML := "name: conn\nnamespace: clearedstack\nimage: conn\ndigest: sha256:3333333333333333333333333333333333333333333333333333333333333333\nreplicas: 1\nport: 3000\nmemory: 128Mi\n"
	if err := os.WriteFile(filepath.Join(connWork, "app.yaml"), []byte(connYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, connWork, "add", ".")
	runGit(t, connWork, "commit", "-m", "conn manifest")
	runGit(t, connWork, "push", connBare, "main:main")

	var uploadPosts atomic.Int32
	cgiHandler := &cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			uploadPosts.Add(1)
		}
		cgiHandler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	client := &plainforge.Client{
		BaseURL: srv.URL,
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
	ctx := context.Background()

	got, err := client.AppManifest(ctx, "sextant")
	if err != nil || string(got) != firstYAML {
		t.Fatalf("first read: err=%v got=%q", err, got)
	}
	if uploadPosts.Load() != 1 {
		t.Fatalf("expected 1 upload-pack POST, got %d", uploadPosts.Load())
	}

	// Second read at the same commit must hit the OID cache with zero pack POSTs.
	got, err = client.AppManifest(ctx, "sextant")
	if err != nil || string(got) != firstYAML {
		t.Fatalf("cached read: err=%v got=%q", err, got)
	}
	if uploadPosts.Load() != 1 {
		t.Fatalf("cached read sent upload-pack POST (%d)", uploadPosts.Load())
	}

	// Push an updated digest and repack aggressively so delta objects are exercised.
	secondYAML := "name: sextant\nnamespace: cs-workers\nimage: sextant\ndigest: sha256:2222222222222222222222222222222222222222222222222222222222222222\nreplicas: 1\nport: 8080\nmemory: 30Mi\n"
	if err := os.WriteFile(filepath.Join(work, "app.yaml"), []byte(secondYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "app.yaml")
	runGit(t, work, "commit", "-m", "bump digest [skip-ci]")
	runGit(t, work, "push", sextantBare, "main:main")
	runGit(t, sextantBare, "repack", "-a", "-d", "--depth=10")

	got, err = client.AppManifest(ctx, "sextant")
	if err != nil || string(got) != secondYAML {
		t.Fatalf("updated read: err=%v got=%q", err, got)
	}

	// Component conn reads from repository clearedstack.com.git.
	gotConn, err := client.AppManifest(ctx, "conn")
	if err != nil || string(gotConn) != connYAML {
		t.Fatalf("conn read: err=%v got=%q", err, gotConn)
	}

	// Empty repo and nonexistent repo both return PlainforgeNoApp.
	if _, err := client.AppManifest(ctx, "trawl"); !errors.Is(err, errs.PlainforgeNoApp) {
		t.Fatalf("empty repo: got %v", err)
	}
	if _, err := client.AppManifest(ctx, "assay"); !errors.Is(err, errs.PlainforgeNoApp) {
		t.Fatalf("missing repo: got %v", err)
	}
}

func TestPlainforgeErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/forbidden.git/info/refs":
			w.WriteHeader(http.StatusForbidden)
		case "/corrupt.git/info/refs":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("not-a-pkt-line"))
		case "/slow.git/info/refs":
			time.Sleep(150 * time.Millisecond)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &plainforge.Client{
		BaseURL: srv.URL,
		HTTP:    &http.Client{Timeout: 30 * time.Millisecond},
	}
	ctx := context.Background()

	if _, err := c.AppManifest(ctx, "forbidden"); !errors.Is(err, errs.PlainforgeRefused) {
		t.Fatalf("403: got %v", err)
	}
	if _, err := c.AppManifest(ctx, "corrupt"); !errors.Is(err, errs.PlainforgeBadReply) {
		t.Fatalf("corrupt: got %v", err)
	}
	if _, err := c.AppManifest(ctx, "slow"); !errors.Is(err, errs.PlainforgeTimeout) {
		t.Fatalf("slow: got %v", err)
	}
	srv.Close()
	if _, err := c.AppManifest(ctx, "sextant"); !errors.Is(err, errs.PlainforgeUnreachable) {
		t.Fatalf("closed: got %v", err)
	}
}
