package lazarette_test

import (
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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/lazarette"
)

const (
	validDigest = "sha256:4e7a4e7a4e7a4e7a4e7a4e7a4e7a4e7a4e7a4e7a4e7a4e7a4e7a4e7a4e7a4e7a"
	otherDigest = "sha256:9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b"
)

func TestHasSignedDigestWithoutKey(t *testing.T) {
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

type signedArtifact struct {
	payloadDigest  string
	payload        []byte
	manifestDigest string
	manifest       []byte
}

func makeCosignSignature(t *testing.T, priv *ecdsa.PrivateKey, signedImageDigest string) signedArtifact {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"critical": map[string]any{
			"identity": map[string]any{"docker-reference": "lazarette.clearedstack.svc.cluster.local:5000/assay"},
			"image":    map[string]any{"docker-manifest-digest": signedImageDigest},
			"type":     "cosign container image signature",
		},
		"optional": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	pSum := sha256.Sum256(payload)
	sigASN1, err := ecdsa.SignASN1(rand.Reader, priv, pSum[:])
	if err != nil {
		t.Fatal(err)
	}
	pDigest := "sha256:" + hex.EncodeToString(pSum[:])
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     lazarette.OCIManifestMediaType,
		"artifactType":  lazarette.SimpleSigningMediaType,
		"layers": []any{
			map[string]any{
				"mediaType": lazarette.SimpleSigningMediaType,
				"digest":    pDigest,
				"size":      len(payload),
				"annotations": map[string]string{
					lazarette.SignatureAnnotation: base64.StdEncoding.EncodeToString(sigASN1),
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mSum := sha256.Sum256(manifest)
	return signedArtifact{
		payloadDigest:  pDigest,
		payload:        payload,
		manifestDigest: "sha256:" + hex.EncodeToString(mSum[:]),
		manifest:       manifest,
	}
}

func marshalPKIXPEM(t *testing.T, pub any) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func TestCosignReferrersAndSigTagVerification(t *testing.T) {
	goodKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	goodSig := makeCosignSignature(t, goodKey, validDigest)
	wrongKeySig := makeCosignSignature(t, wrongKey, validDigest)
	replaySig := makeCosignSignature(t, goodKey, otherDigest) // valid key, wrong target digest

	sigTag := "sha256-" + strings.TrimPrefix(validDigest, "sha256:") + ".sig"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// All five test repositories hold the image manifest itself.
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/manifests/"+validDigest):
			w.Header().Set("Docker-Content-Digest", validDigest)
			w.WriteHeader(http.StatusOK)

		// 1. "assay": valid signature served via OCI 1.1 referrers.
		case r.URL.Path == "/v2/assay/referrers/"+validDigest:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schemaVersion": 2,
				"mediaType":     lazarette.OCIIndexMediaType,
				"manifests": []any{
					map[string]any{
						"mediaType":    lazarette.OCIManifestMediaType,
						"digest":       goodSig.manifestDigest,
						"artifactType": lazarette.SimpleSigningMediaType,
					},
				},
			})
		case r.URL.Path == "/v2/assay/manifests/"+goodSig.manifestDigest:
			_, _ = w.Write(goodSig.manifest)
		case r.URL.Path == "/v2/assay/blobs/"+goodSig.payloadDigest:
			_, _ = w.Write(goodSig.payload)

		// 2. "legacy": referrers returns 404; valid signature served via .sig tag.
		case r.URL.Path == "/v2/legacy/manifests/"+sigTag:
			_, _ = w.Write(goodSig.manifest)
		case r.URL.Path == "/v2/legacy/blobs/"+goodSig.payloadDigest:
			_, _ = w.Write(goodSig.payload)

		// 3. "unsigned": image manifest exists, but no referrers and no .sig tag.
		case r.URL.Path == "/v2/unsigned/referrers/"+validDigest:
			_ = json.NewEncoder(w).Encode(map[string]any{"schemaVersion": 2, "manifests": []any{}})

		// 4. "forged": signed by an unknown ECDSA P-256 key.
		case r.URL.Path == "/v2/forged/referrers/"+validDigest:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schemaVersion": 2,
				"manifests": []any{
					map[string]any{
						"mediaType":    lazarette.OCIManifestMediaType,
						"digest":       wrongKeySig.manifestDigest,
						"artifactType": lazarette.SimpleSigningMediaType,
					},
				},
			})
		case r.URL.Path == "/v2/forged/manifests/"+wrongKeySig.manifestDigest:
			_, _ = w.Write(wrongKeySig.manifest)
		case r.URL.Path == "/v2/forged/blobs/"+wrongKeySig.payloadDigest:
			_, _ = w.Write(wrongKeySig.payload)

		// 5. "replayed": signed by goodKey, but payload binds otherDigest instead of validDigest.
		case r.URL.Path == "/v2/replayed/referrers/"+validDigest:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schemaVersion": 2,
				"manifests": []any{
					map[string]any{
						"mediaType":    lazarette.OCIManifestMediaType,
						"digest":       replaySig.manifestDigest,
						"artifactType": lazarette.SimpleSigningMediaType,
					},
				},
			})
		case r.URL.Path == "/v2/replayed/manifests/"+replaySig.manifestDigest:
			_, _ = w.Write(replaySig.manifest)
		case r.URL.Path == "/v2/replayed/blobs/"+replaySig.payloadDigest:
			_, _ = w.Write(replaySig.payload)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	pubPath := filepath.Join(t.TempDir(), "cosign.pub")
	if err := os.WriteFile(pubPath, marshalPKIXPEM(t, &goodKey.PublicKey), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &lazarette.Client{
		BaseURL:       srv.URL,
		HTTP:          &http.Client{Timeout: 2 * time.Second},
		PublicKeyFile: pubPath,
	}
	ctx := context.Background()

	for _, accepted := range []string{"assay", "legacy"} {
		ok, err := c.HasSignedDigest(ctx, accepted, validDigest)
		if err != nil || !ok {
			t.Errorf("%s: got ok=%v err=%v, want true, nil", accepted, ok, err)
		}
	}

	for _, refused := range []string{"unsigned", "forged", "replayed"} {
		ok, err := c.HasSignedDigest(ctx, refused, validDigest)
		if err != nil || ok {
			t.Errorf("%s: got ok=%v err=%v, want false, nil", refused, ok, err)
		}
	}

	// Rotating PublicKeyFile on disk switches verification immediately.
	if err := os.WriteFile(pubPath, marshalPKIXPEM(t, &wrongKey.PublicKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.HasSignedDigest(ctx, "forged", validDigest); err != nil || !ok {
		t.Fatalf("after key rotation: ok=%v err=%v", ok, err)
	}
}

func TestParsePublicKeyRejectsNonP256AndMalformedPEM(t *testing.T) {
	if _, err := lazarette.ParsePublicKey([]byte("not pem")); !errors.Is(err, errs.Config) {
		t.Fatalf("garbage PEM: got %v", err)
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lazarette.ParsePublicKey(marshalPKIXPEM(t, &p384.PublicKey)); !errors.Is(err, errs.Config) {
		t.Fatalf("P-384 key: got %v, want %v", err, errs.Config)
	}
}
