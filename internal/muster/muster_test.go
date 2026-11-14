package muster_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/muster"
)

func TestTokenFileReadOnEveryCall(t *testing.T) {
	mem := &muster.Memory{
		Tokens: map[string]string{
			"first-token":  "shipkeeper",
			"second-token": "shipkeeper",
		},
	}
	srv := httptest.NewServer(mem)
	defer srv.Close()

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("first-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &muster.Client{
		BaseURL:   srv.URL,
		TokenFile: tokenFile,
		HTTP:      &http.Client{Timeout: 2 * time.Second},
	}
	ctx := context.Background()
	if _, err := c.SendHealth(ctx, []muster.Report{{Component: "shipkeeper", State: muster.Healthy}}); err != nil {
		t.Fatalf("first token: %v", err)
	}

	// Rotate the ServiceAccount token on disk and revoke the old one.
	delete(mem.Tokens, "first-token")
	if err := os.WriteFile(tokenFile, []byte("second-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendHealth(ctx, []muster.Report{{Component: "shipkeeper", State: muster.Healthy}}); err != nil {
		t.Fatalf("rotated token: %v", err)
	}
}

func TestTransportTimeoutAndUnreachableAreRetryable(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer slow.Close()

	c := &muster.Client{
		BaseURL: slow.URL,
		Token:   "token-shipkeeper",
		HTTP:    &http.Client{Timeout: 30 * time.Millisecond},
	}
	_, err := c.SendHealth(context.Background(), []muster.Report{{Component: "shipkeeper", State: muster.Healthy}})
	if !errors.Is(err, errs.MusterTimeout) || !muster.Retryable(err) {
		t.Fatalf("slow Muster: err=%v retryable=%v", err, muster.Retryable(err))
	}

	slow.Close()
	_, err = c.SendHealth(context.Background(), []muster.Report{{Component: "shipkeeper", State: muster.Healthy}})
	if !errors.Is(err, errs.MusterUnreachable) || !muster.Retryable(err) {
		t.Fatalf("closed Muster: err=%v retryable=%v", err, muster.Retryable(err))
	}
}

func TestMalformedAndDownResponses(t *testing.T) {
	mem := &muster.Memory{Down: true}
	srv := httptest.NewServer(mem)
	defer srv.Close()

	c := &muster.Client{
		BaseURL: srv.URL,
		Token:   "token-shipkeeper",
		HTTP:    &http.Client{Timeout: 2 * time.Second},
	}
	_, err := c.SendHealth(context.Background(), []muster.Report{{Component: "shipkeeper", State: muster.Healthy}})
	if !errors.Is(err, errs.MusterBusy) || !muster.Retryable(err) {
		t.Fatalf("down Muster: err=%v retryable=%v", err, muster.Retryable(err))
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"unexpected": true}`))
	}))
	defer bad.Close()
	c.BaseURL = bad.URL
	_, err = c.SendHealth(context.Background(), []muster.Report{{Component: "shipkeeper", State: muster.Healthy}})
	if !errors.Is(err, errs.MusterBadReply) || muster.Retryable(err) {
		t.Fatalf("bad reply: err=%v retryable=%v", err, muster.Retryable(err))
	}
}
