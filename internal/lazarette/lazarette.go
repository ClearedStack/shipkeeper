// Package lazarette checks whether Lazarette holds a given image manifest by
// immutable sha256 digest through the OCI Distribution v2 HTTP API and, when a
// public key is configured, verifies its ECDSA P-256 cosign SimpleSigning
// signature via OCI 1.1 referrers (with sha256-<hex>.sig fallback).
package lazarette

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/ClearedStack/shipkeeper/internal/errs"
)

// OCI and cosign SimpleSigning media types and annotation keys.
const (
	OCIManifestMediaType   = "application/vnd.oci.image.manifest.v1+json"
	OCIIndexMediaType      = "application/vnd.oci.image.index.v1+json"
	SimpleSigningMediaType = "application/vnd.dev.cosign.simplesigning.v1+json"
	SignatureAnnotation    = "dev.cosignproject.cosign/signature"
	cosignPayloadType      = "cosign container image signature"
)

const (
	// MaxRetainedDigests is the maximum number of retained signed digests
	// Lazarette returns from GET /v2/<name>/digests/list (SPEC.md).
	MaxRetainedDigests = 5

	acceptManifest   = OCIManifestMediaType + ", " + OCIIndexMediaType
	maxErrBytes      = 16 << 10
	maxListBytes     = 16 << 10
	maxManifestBytes = 64 << 10
	maxPayloadBytes  = 64 << 10
	maxReferrers     = 16
	maxSigLayers     = 8
)

var (
	imageRE  = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)
	digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ParsePublicKey parses an ECDSA P-256 public key in PKIX PEM form.
func ParsePublicKey(pemData []byte) (*ecdsa.PublicKey, error) {
	block, rest := pem.Decode(pemData)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 || block.Type != "PUBLIC KEY" {
		return nil, errs.Config
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errs.Config
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, errs.Config
	}
	return pub, nil
}

// Client queries Lazarette over cluster-private HTTP.
type Client struct {
	BaseURL       string           // e.g. http://lazarette.clearedstack.svc.cluster.local:5000
	HTTP          *http.Client     // must carry a timeout
	PublicKey     *ecdsa.PublicKey // optional static ECDSA P-256 verification key
	PublicKeyFile string           // optional path to PKIX PEM public key, read fresh per call
}

// HasSignedDigest checks HEAD /v2/<image>/manifests/<digest> on Lazarette and,
// when a cosign ECDSA P-256 public key is configured, verifies at least one
// valid SimpleSigning signature attached to digest.
func (c *Client) HasSignedDigest(ctx context.Context, image, digest string) (bool, error) {
	if c.BaseURL == "" || c.HTTP == nil {
		return false, errs.Config
	}
	if !imageRE.MatchString(image) || !digestRE.MatchString(digest) {
		return false, errs.ManifestValue
	}
	pub, err := c.publicKey()
	if err != nil {
		return false, err
	}
	return c.hasSignedDigestWithKey(ctx, pub, image, digest)
}

// RetainedDigests fetches GET /v2/<image>/digests/list from Lazarette in
// oldest-to-newest order (at most MaxRetainedDigests) and filters to digests
// whose manifest and cosign signature verify cleanly.
func (c *Client) RetainedDigests(ctx context.Context, image string) ([]string, error) {
	if c.BaseURL == "" || c.HTTP == nil {
		return nil, errs.Config
	}
	if !imageRE.MatchString(image) {
		return nil, errs.ManifestValue
	}
	pub, err := c.publicKey()
	if err != nil {
		return nil, err
	}

	status, _, body, err := c.do(ctx, http.MethodGet, "/v2/"+image+"/digests/list", "application/json", maxListBytes)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, nil
	default:
		return nil, statusError(status)
	}

	var reply struct {
		Name    string   `json:"name"`
		Digests []string `json:"digests"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&reply); err != nil {
		return nil, errs.LazaretteBadReply
	}
	if reply.Name != "" && reply.Name != image {
		return nil, errs.LazaretteBadReply
	}
	if len(reply.Digests) > MaxRetainedDigests {
		return nil, errs.LazaretteBadReply
	}

	var verified []string
	seen := make(map[string]bool, len(reply.Digests))
	for _, d := range reply.Digests {
		if !digestRE.MatchString(d) || seen[d] {
			return nil, errs.LazaretteBadReply
		}
		seen[d] = true
		ok, err := c.hasSignedDigestWithKey(ctx, pub, image, d)
		if err != nil {
			return nil, err
		}
		if ok {
			verified = append(verified, d)
		}
	}
	return verified, nil
}

func (c *Client) hasSignedDigestWithKey(ctx context.Context, pub *ecdsa.PublicKey, image, digest string) (bool, error) {
	status, hdr, _, err := c.do(ctx, http.MethodHead, "/v2/"+image+"/manifests/"+digest, acceptManifest, 0)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
		if got := hdr.Get("Docker-Content-Digest"); got != "" && got != digest {
			return false, errs.LazaretteBadReply
		}
	case http.StatusNotFound:
		return false, nil
	default:
		return false, statusError(status)
	}

	if pub == nil {
		return true, nil
	}
	return c.verifySignature(ctx, pub, image, digest)
}

func (c *Client) publicKey() (*ecdsa.PublicKey, error) {
	if c.PublicKeyFile == "" {
		return c.PublicKey, nil
	}
	raw, err := os.ReadFile(c.PublicKeyFile)
	if err != nil {
		return nil, errs.Config
	}
	return ParsePublicKey(raw)
}

type ociIndex struct {
	Manifests []struct {
		MediaType    string `json:"mediaType"`
		Digest       string `json:"digest"`
		ArtifactType string `json:"artifactType"`
	} `json:"manifests"`
}

type ociManifest struct {
	SchemaVersion int `json:"schemaVersion"`
	Layers        []struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Annotations map[string]string `json:"annotations"`
	} `json:"layers"`
}

type simpleSigning struct {
	Critical struct {
		Image struct {
			DockerManifestDigest string `json:"docker-manifest-digest"`
		} `json:"image"`
		Type string `json:"type"`
	} `json:"critical"`
}

func (c *Client) verifySignature(ctx context.Context, pub *ecdsa.PublicKey, image, digest string) (bool, error) {
	manifests, err := c.signatureManifests(ctx, image, digest)
	if err != nil || len(manifests) == 0 {
		return false, err
	}
	for _, rawManifest := range manifests {
		ok, err := c.verifyManifest(ctx, pub, image, digest, rawManifest)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) signatureManifests(ctx context.Context, image, digest string) ([][]byte, error) {
	refPath := "/v2/" + image + "/referrers/" + digest + "?artifactType=" + url.QueryEscape(SimpleSigningMediaType)
	status, _, body, err := c.do(ctx, http.MethodGet, refPath, OCIIndexMediaType, maxManifestBytes)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		var idx ociIndex
		if json.Unmarshal(body, &idx) != nil || len(idx.Manifests) > maxReferrers {
			return nil, errs.LazaretteBadReply
		}
		var out [][]byte
		for _, desc := range idx.Manifests {
			if !digestRE.MatchString(desc.Digest) {
				continue
			}
			if desc.ArtifactType != "" && desc.ArtifactType != SimpleSigningMediaType {
				continue
			}
			st, _, mbody, err := c.do(ctx, http.MethodGet, "/v2/"+image+"/manifests/"+desc.Digest, OCIManifestMediaType, maxManifestBytes)
			if err != nil {
				return nil, err
			}
			switch st {
			case http.StatusOK:
				if sha256Digest(mbody) == desc.Digest {
					out = append(out, mbody)
				}
			case http.StatusNotFound:
				continue
			default:
				return nil, statusError(st)
			}
		}
		return out, nil
	case http.StatusNotFound, http.StatusBadRequest, http.StatusUnprocessableEntity:
		// Fall back to cosign's sha256-<hex>.sig tag layout when OCI 1.1
		// referrers are not served for this repository.
	default:
		return nil, statusError(status)
	}

	sigTag := "sha256-" + strings.TrimPrefix(digest, "sha256:") + ".sig"
	st, _, mbody, err := c.do(ctx, http.MethodGet, "/v2/"+image+"/manifests/"+sigTag, OCIManifestMediaType, maxManifestBytes)
	if err != nil {
		return nil, err
	}
	switch st {
	case http.StatusOK:
		return [][]byte{mbody}, nil
	case http.StatusNotFound:
		return nil, nil
	default:
		return nil, statusError(st)
	}
}

func (c *Client) verifyManifest(ctx context.Context, pub *ecdsa.PublicKey, image, digest string, rawManifest []byte) (bool, error) {
	var m ociManifest
	if json.Unmarshal(rawManifest, &m) != nil || m.SchemaVersion != 2 || len(m.Layers) == 0 || len(m.Layers) > maxSigLayers {
		return false, nil
	}
	for _, layer := range m.Layers {
		if layer.MediaType != SimpleSigningMediaType || !digestRE.MatchString(layer.Digest) {
			continue
		}
		sigB64 := layer.Annotations[SignatureAnnotation]
		if sigB64 == "" {
			continue
		}
		sigBytes, err := base64.StdEncoding.DecodeString(sigB64)
		if err != nil || len(sigBytes) == 0 {
			continue
		}
		st, _, payload, err := c.do(ctx, http.MethodGet, "/v2/"+image+"/blobs/"+layer.Digest, SimpleSigningMediaType, maxPayloadBytes)
		if err != nil {
			return false, err
		}
		switch st {
		case http.StatusOK:
		case http.StatusNotFound:
			continue
		default:
			return false, statusError(st)
		}
		if sha256Digest(payload) != layer.Digest {
			continue
		}
		sum := sha256.Sum256(payload)
		if !ecdsa.VerifyASN1(pub, sum[:], sigBytes) {
			continue
		}
		var ss simpleSigning
		if json.Unmarshal(payload, &ss) != nil {
			continue
		}
		if ss.Critical.Type == cosignPayloadType && ss.Critical.Image.DockerManifestDigest == digest {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) do(ctx context.Context, method, path, accept string, limit int64) (int, http.Header, []byte, error) {
	endpoint := strings.TrimRight(c.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return 0, nil, nil, errs.Config
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, nil, transportError(err)
	}
	defer resp.Body.Close()
	if method == http.MethodHead {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrBytes))
		return resp.StatusCode, resp.Header, nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return 0, nil, nil, transportError(err)
	}
	if int64(len(body)) > limit {
		return 0, nil, nil, errs.LazaretteBadReply
	}
	return resp.StatusCode, resp.Header, body, nil
}

func statusError(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return errs.LazaretteRefused
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return errs.LazaretteTimeout
	default:
		return errs.LazaretteBadReply
	}
}

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func transportError(err error) error {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return errs.LazaretteTimeout
	}
	return errs.LazaretteUnreachable
}
