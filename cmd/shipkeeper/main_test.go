package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ClearedStack/shipkeeper/internal/example"
	"github.com/ClearedStack/shipkeeper/internal/golden"
	"github.com/ClearedStack/shipkeeper/internal/kubetest"
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
