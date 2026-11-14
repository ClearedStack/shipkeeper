package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
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
	"github.com/ClearedStack/shipkeeper/internal/lazarette"
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

	cosignPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&cosignPriv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	cosignPubFile := filepath.Join(t.TempDir(), "cosign.pub")
	if err := os.WriteFile(cosignPubFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	sigPayload, _ := json.Marshal(map[string]any{
		"critical": map[string]any{
			"identity": map[string]any{"docker-reference": "lazarette.clearedstack.svc.cluster.local:5000/assay"},
			"image":    map[string]any{"docker-manifest-digest": assayDigest},
			"type":     "cosign container image signature",
		},
	})
	pSum := sha256.Sum256(sigPayload)
	sigASN1, err := ecdsa.SignASN1(rand.Reader, cosignPriv, pSum[:])
	if err != nil {
		t.Fatal(err)
	}
	pDigest := "sha256:" + hex.EncodeToString(pSum[:])
	sigManifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     lazarette.OCIManifestMediaType,
		"artifactType":  lazarette.SimpleSigningMediaType,
		"layers": []any{map[string]any{
			"mediaType":   lazarette.SimpleSigningMediaType,
			"digest":      pDigest,
			"annotations": map[string]string{lazarette.SignatureAnnotation: base64.StdEncoding.EncodeToString(sigASN1)},
		}},
	})
	mSum := sha256.Sum256(sigManifest)
	mDigest := "sha256:" + hex.EncodeToString(mSum[:])

	lazaretteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/v2/assay/manifests/"+assayDigest:
			w.Header().Set("Docker-Content-Digest", assayDigest)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/assay/referrers/"+assayDigest:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schemaVersion": 2,
				"manifests": []any{map[string]any{
					"mediaType":    lazarette.OCIManifestMediaType,
					"digest":       mDigest,
					"artifactType": lazarette.SimpleSigningMediaType,
				}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v2/assay/manifests/"+mDigest:
			_, _ = w.Write(sigManifest)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/assay/blobs/"+pDigest:
			_, _ = w.Write(sigPayload)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
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
		"-cosign-pub-file", cosignPubFile,
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

func TestLifecycleSubcommandsEndToEnd(t *testing.T) {
	const backend = "/usr/lib/git-core/git-http-backend"
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend not installed")
	}

	const (
		d1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		d2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	)

	gitRoot := t.TempDir()
	assayBare := filepath.Join(gitRoot, "assay.git")
	gitCmd := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Shipkeeper",
			"GIT_AUTHOR_EMAIL=test@example.invalid",
			"GIT_COMMITTER_NAME=Shipkeeper",
			"GIT_COMMITTER_EMAIL=test@example.invalid",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	gitCmd(gitRoot, "init", "--bare", "--initial-branch=main", assayBare)
	gitCmd(assayBare, "config", "http.uploadpack", "true")
	gitCmd(assayBare, "config", "http.receivepack", "true")

	work := filepath.Join(gitRoot, "work")
	gitCmd(gitRoot, "init", "--initial-branch=main", work)
	initialYAML := "name: assay\nnamespace: cs-workers\nimage: assay\ndigest: " + d1 + "\nreplicas: 1\nport: 8080\nmemory: 32Mi\n"
	if err := os.WriteFile(filepath.Join(work, "app.yaml"), []byte(initialYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(work, "add", ".")
	gitCmd(work, "commit", "-m", "initial assay")
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

	cosignPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&cosignPriv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	cosignPubFile := filepath.Join(t.TempDir(), "cosign.pub")
	if err := os.WriteFile(cosignPubFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o600); err != nil {
		t.Fatal(err)
	}

	type sigRecord struct {
		mDigest string
		pDigest string
		mBytes  []byte
		pBytes  []byte
	}
	makeSig := func(digest string) sigRecord {
		payload, _ := json.Marshal(map[string]any{
			"critical": map[string]any{
				"identity": map[string]any{"docker-reference": "lazarette.clearedstack.svc.cluster.local:5000/assay"},
				"image":    map[string]any{"docker-manifest-digest": digest},
				"type":     "cosign container image signature",
			},
		})
		pSum := sha256.Sum256(payload)
		sigASN1, err := ecdsa.SignASN1(rand.Reader, cosignPriv, pSum[:])
		if err != nil {
			t.Fatal(err)
		}
		pDigest := "sha256:" + hex.EncodeToString(pSum[:])
		manifestBytes, _ := json.Marshal(map[string]any{
			"schemaVersion": 2,
			"mediaType":     lazarette.OCIManifestMediaType,
			"artifactType":  lazarette.SimpleSigningMediaType,
			"layers": []any{map[string]any{
				"mediaType":   lazarette.SimpleSigningMediaType,
				"digest":      pDigest,
				"annotations": map[string]string{lazarette.SignatureAnnotation: base64.StdEncoding.EncodeToString(sigASN1)},
			}},
		})
		mSum := sha256.Sum256(manifestBytes)
		return sigRecord{
			mDigest: "sha256:" + hex.EncodeToString(mSum[:]),
			pDigest: pDigest,
			mBytes:  manifestBytes,
			pBytes:  payload,
		}
	}
	sigs := map[string]sigRecord{
		d1: makeSig(d1),
		d2: makeSig(d2),
	}

	lazaretteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/assay/digests/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name":    "assay",
				"digests": []string{d1, d2},
			})
		case r.Method == http.MethodHead && (r.URL.Path == "/v2/assay/manifests/"+d1 || r.URL.Path == "/v2/assay/manifests/"+d2):
			d := filepath.Base(r.URL.Path)
			w.Header().Set("Docker-Content-Digest", d)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && (r.URL.Path == "/v2/assay/referrers/"+d1 || r.URL.Path == "/v2/assay/referrers/"+d2):
			rec := sigs[filepath.Base(r.URL.Path)]
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schemaVersion": 2,
				"manifests": []any{map[string]any{
					"mediaType":    lazarette.OCIManifestMediaType,
					"digest":       rec.mDigest,
					"artifactType": lazarette.SimpleSigningMediaType,
				}},
			})
		default:
			for _, rec := range sigs {
				if r.Method == http.MethodGet && r.URL.Path == "/v2/assay/manifests/"+rec.mDigest {
					_, _ = w.Write(rec.mBytes)
					return
				}
				if r.Method == http.MethodGet && r.URL.Path == "/v2/assay/blobs/"+rec.pDigest {
					_, _ = w.Write(rec.pBytes)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer lazaretteSrv.Close()

	memMuster := &muster.Memory{
		Tokens: map[string]string{"shipkeeper-token": "shipkeeper"},
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

	commonFlags := []string{
		"-app", "assay",
		"-plainforge-url", plainforgeSrv.URL,
		"-lazarette-url", lazaretteSrv.URL,
		"-cosign-pub-file", cosignPubFile,
		"-muster-url", musterSrv.URL,
		"-kube-url", cluster.URL,
		"-kube-token-file", tokFile,
	}

	execSub := func(sub string, extra ...string) map[string]any {
		t.Helper()
		var stdout, stderr bytes.Buffer
		args := append([]string{sub}, commonFlags...)
		args = append(args, extra...)
		if code := runContext(context.Background(), args, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
			t.Fatalf("%s failed: exit=%d stderr=%q", sub, code, stderr.String())
		}
		var out map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatalf("%s stdout JSON: %v\n%s", sub, err, stdout.String())
		}
		return out
	}

	// 1. Pause sets replicas=0 and reports state="paused".
	pauseRep := execSub("pause")
	if pauseRep["state"] != "paused" || cluster.Field("cs-workers", "assay", "spec.replicas") != 0.0 {
		t.Fatalf("after pause: report=%v replicas=%v", pauseRep, cluster.Field("cs-workers", "assay", "spec.replicas"))
	}

	// 2. Resume scales replicas=2.
	_ = execSub("resume", "-replicas", "2")
	if cluster.Field("cs-workers", "assay", "spec.replicas") != 2.0 {
		t.Fatalf("after resume: replicas=%v", cluster.Field("cs-workers", "assay", "spec.replicas"))
	}

	// 3. Upgrade pins newest signed digest d2.
	_ = execSub("upgrade")
	showUpgraded := gitCmd(assayBare, "show", "refs/heads/main:app.yaml")
	if !bytes.Contains([]byte(showUpgraded), []byte("digest: "+d2)) {
		t.Fatalf("expected d2 in Plainforge app.yaml, got:\n%s", showUpgraded)
	}

	// 4. Rollback steps back to d1.
	_ = execSub("rollback")
	showRolledBack := gitCmd(assayBare, "show", "refs/heads/main:app.yaml")
	if !bytes.Contains([]byte(showRolledBack), []byte("digest: "+d1)) {
		t.Fatalf("expected d1 after rollback, got:\n%s", showRolledBack)
	}

	// Verify bare repository passes git fsck --strict after all 4 [skip-ci] commits.
	gitCmd(assayBare, "fsck", "--strict")
}
