package lazarette_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/lazarette"
)

const validDigest = "sha256:4e7a4e7a4e7a4e7a4e7a4e7a4e7a4e7a4e7a4e7a4e7a4e7a4e7a4e7a4e7a4e7a"

func TestHasSignedDigest(t *testing.T) {
	var gotMethod, gotPath, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAccept = r.Method, r.URL.Path, r.Header.Get("Accept")
		switch r.URL.Path {
		case "/v2/tools/sextant/manifests/" + validDigest:
			w.Header().Set("Docker-Content-Digest", validDigest)
			w.WriteHeader(http.StatusOK)
		case "/v2/mismatch/manifests/" + validDigest:
			w.Header().Set("Docker-Content-Digest", "sha256:0000000000000000000000000000000000000000000000000000000000000000")
			w.WriteHeader(http.StatusOK)
		case "/v2/forbidden/manifests/" + validDigest:
			w.WriteHeader(http.StatusForbidden)
		case "/v2/busy/manifests/" + validDigest:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &lazarette.Client{
		BaseURL: srv.URL,
		HTTP:    &http.Client{Timeout: 2 * time.Second},
	}
	ctx := context.Background()

	ok, err := c.HasSignedDigest(ctx, "tools/sextant", validDigest)
	if err != nil || !ok {
		t.Fatalf("present digest: ok=%v err=%v", ok, err)
	}
	if gotMethod != http.MethodHead || gotPath != "/v2/tools/sextant/manifests/"+validDigest || gotAccept == "" {
		t.Fatalf("unexpected request: %s %s accept=%q", gotMethod, gotPath, gotAccept)
	}

	ok, err = c.HasSignedDigest(ctx, "sextant", validDigest)
	if err != nil || ok {
		t.Fatalf("missing digest: ok=%v err=%v", ok, err)
	}

	if _, err := c.HasSignedDigest(ctx, "mismatch", validDigest); !errors.Is(err, errs.LazaretteBadReply) {
		t.Fatalf("mismatched header: got %v", err)
	}
	if _, err := c.HasSignedDigest(ctx, "forbidden", validDigest); !errors.Is(err, errs.LazaretteRefused) {
		t.Fatalf("forbidden: got %v", err)
	}
	if _, err := c.HasSignedDigest(ctx, "busy", validDigest); !errors.Is(err, errs.LazaretteTimeout) {
		t.Fatalf("503: got %v", err)
	}

	srv.Close()
	if _, err := c.HasSignedDigest(ctx, "sextant", validDigest); !errors.Is(err, errs.LazaretteUnreachable) {
		t.Fatalf("closed server: got %v", err)
	}
}
