package plainforge_test

import (
	"context"
	"errors"
	"io"
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

func TestUpdateAppYAMLPreservesCommentsAndValidates(t *testing.T) {
	orig := "# header comment\nname: sextant\nnamespace: cs-workers\nimage: sextant\ndigest: sha256:1111111111111111111111111111111111111111111111111111111111111111 # pinned\nreplicas: 2 # baseline\nport: 8080\nmemory: 30Mi\n"
	zero := 0
	nextDigest := "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	updated, changed, err := plainforge.UpdateAppYAML([]byte(orig), nextDigest, &zero)
	if err != nil {
		t.Fatalf("UpdateAppYAML: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true")
	}
	want := "# header comment\nname: sextant\nnamespace: cs-workers\nimage: sextant\ndigest: " + nextDigest + " # pinned\nreplicas: 0 # baseline\nport: 8080\nmemory: 30Mi\n"
	if string(updated) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", updated, want)
	}

	again, changed, err := plainforge.UpdateAppYAML(updated, nextDigest, &zero)
	if err != nil || changed || string(again) != want {
		t.Fatalf("idempotent UpdateAppYAML: err=%v changed=%v", err, changed)
	}

	bad := -1
	if _, _, err := plainforge.UpdateAppYAML(updated, "", &bad); !errors.Is(err, errs.ManifestValue) {
		t.Fatalf("expected ManifestValue, got %v", err)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestUpdateAppManifestOverGitSmartHTTP(t *testing.T) {
	const backend = "/usr/lib/git-core/git-http-backend"
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend not installed")
	}

	root := t.TempDir()
	sextantBare := filepath.Join(root, "sextant.git")
	runGit(t, root, "init", "--bare", "--initial-branch=main", sextantBare)
	runGit(t, sextantBare, "config", "http.uploadpack", "true")
	runGit(t, sextantBare, "config", "http.receivepack", "true")

	connBare := filepath.Join(root, "clearedstack.com.git")
	runGit(t, root, "init", "--bare", "--initial-branch=main", connBare)
	runGit(t, connBare, "config", "http.uploadpack", "true")
	runGit(t, connBare, "config", "http.receivepack", "true")

	// Seed sextant.git with directories and files that exercise Git's tree sort rule ("app-note.txt" < "app/" < "app.yaml" etc.).
	work := filepath.Join(root, "work")
	runGit(t, root, "init", "--initial-branch=main", work)
	initialSextant := "name: sextant\nnamespace: cs-workers\nimage: sextant\ndigest: sha256:1111111111111111111111111111111111111111111111111111111111111111\nreplicas: 1\nport: 8080\nmemory: 30Mi\n"
	if err := os.WriteFile(filepath.Join(work, "app.yaml"), []byte(initialSextant), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "app-notes.txt"), []byte("notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "app", "keep.txt"), []byte("dir\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "initial sextant")
	runGit(t, work, "push", sextantBare, "main:main")

	// Seed clearedstack.com.git with apps/conn/app.yaml to exercise nested multi-app tree updates.
	connWork := filepath.Join(root, "conn-work")
	runGit(t, root, "init", "--initial-branch=main", connWork)
	if err := os.MkdirAll(filepath.Join(connWork, "apps", "conn"), 0o755); err != nil {
		t.Fatal(err)
	}
	initialConn := "name: conn\nnamespace: clearedstack\nimage: conn\ndigest: sha256:3333333333333333333333333333333333333333333333333333333333333333\nreplicas: 1\nport: 3000\nmemory: 128Mi\n"
	if err := os.WriteFile(filepath.Join(connWork, "apps", "conn", "app.yaml"), []byte(initialConn), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(connWork, "apps.md"), []byte("apps doc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, connWork, "add", ".")
	runGit(t, connWork, "commit", "-m", "initial conn")
	runGit(t, connWork, "push", connBare, "main:main")

	cgiHandler := &cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
		},
		Stderr: io.Discard,
	}
	srv := httptest.NewServer(cgiHandler)
	defer srv.Close()

	client := &plainforge.Client{
		BaseURL: srv.URL,
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
	ctx := context.Background()

	zero := 0
	pausedSextant, _, err := plainforge.UpdateAppYAML(
		[]byte(initialSextant),
		"sha256:2222222222222222222222222222222222222222222222222222222222222222",
		&zero,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.UpdateAppManifest(ctx, "sextant", pausedSextant, "shipkeeper: upgrade sextant"); err != nil {
		t.Fatalf("UpdateAppManifest sextant: %v", err)
	}

	got, err := client.AppManifest(ctx, "sextant")
	if err != nil || string(got) != string(pausedSextant) {
		t.Fatalf("AppManifest after update: err=%v got=%q", err, got)
	}

	logMsg := gitOutput(t, sextantBare, "log", "-1", "--format=%B", "refs/heads/main")
	if logMsg != "shipkeeper: upgrade sextant [skip-ci]\n\n" && logMsg != "shipkeeper: upgrade sextant [skip-ci]\n" {
		t.Fatalf("unexpected commit message: %q", logMsg)
	}

	// Idempotent re-apply with identical content creates zero extra commits.
	countBefore := gitOutput(t, sextantBare, "rev-list", "--count", "refs/heads/main")
	if err := client.UpdateAppManifest(ctx, "sextant", pausedSextant, "shipkeeper: no-op"); err != nil {
		t.Fatalf("idempotent UpdateAppManifest: %v", err)
	}
	countAfter := gitOutput(t, sextantBare, "rev-list", "--count", "refs/heads/main")
	if countBefore != countAfter {
		t.Fatalf("idempotent update created a commit: %s -> %s", countBefore, countAfter)
	}

	// Update nested apps/conn/app.yaml in clearedstack.com.git.
	upgradedConn, _, err := plainforge.UpdateAppYAML(
		[]byte(initialConn),
		"sha256:4444444444444444444444444444444444444444444444444444444444444444",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.UpdateAppManifest(ctx, "conn", upgradedConn, "shipkeeper: upgrade conn [skip-ci]"); err != nil {
		t.Fatalf("UpdateAppManifest conn: %v", err)
	}
	gotConn, err := client.AppManifest(ctx, "conn")
	if err != nil || string(gotConn) != string(upgradedConn) {
		t.Fatalf("AppManifest conn after update: err=%v got=%q", err, gotConn)
	}

	// Strict fsck on both bare repositories must pass with zero object/tree sorting issues.
	runGit(t, sextantBare, "fsck", "--strict")
	runGit(t, connBare, "fsck", "--strict")

	// Simulate concurrent ref races rejecting git-receive-pack on all 3 attempts.
	var conflictPushes atomic.Int32
	conflictSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/sextant.git/git-receive-pack" {
			conflictPushes.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("000eunpack ok\n0029ng refs/heads/main non-fast-forward\n0000"))
			return
		}
		cgiHandler.ServeHTTP(w, r)
	}))
	defer conflictSrv.Close()

	conflictClient := &plainforge.Client{
		BaseURL: conflictSrv.URL,
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
	one := 1
	resumedSextant, _, err := plainforge.UpdateAppYAML(pausedSextant, "", &one)
	if err != nil {
		t.Fatal(err)
	}
	if err := conflictClient.UpdateAppManifest(ctx, "sextant", resumedSextant, "shipkeeper: resume sextant"); !errors.Is(err, errs.PlainforgeConflict) {
		t.Fatalf("expected PlainforgeConflict, got %v", err)
	}
	if conflictPushes.Load() != 3 {
		t.Fatalf("expected 3 push attempts, got %d", conflictPushes.Load())
	}
}
