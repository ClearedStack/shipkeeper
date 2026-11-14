// Package lazarette checks whether Lazarette holds a given image manifest by
// immutable sha256 digest through the OCI Distribution v2 HTTP API.
package lazarette

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"

	"github.com/ClearedStack/shipkeeper/internal/errs"
)

const (
	acceptManifest = "application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json"
	maxErrBytes    = 16 << 10
)

var (
	imageRE  = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)
	digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Client queries Lazarette over cluster-private HTTP.
type Client struct {
	BaseURL string       // e.g. http://lazarette.clearedstack.svc.cluster.local:5000
	HTTP    *http.Client // must carry a timeout
}

// HasSignedDigest checks HEAD /v2/<image>/manifests/<digest> on Lazarette.
//
// Lazarette currently stores OCI manifests by digest without cosign signature
// artifacts; see README.md for the signature verification dependency on
// Keelworks and Lazarette.
func (c *Client) HasSignedDigest(ctx context.Context, image, digest string) (bool, error) {
	if c.BaseURL == "" || c.HTTP == nil {
		return false, errs.Config
	}
	if !imageRE.MatchString(image) || !digestRE.MatchString(digest) {
		return false, errs.ManifestValue
	}

	endpoint := strings.TrimRight(c.BaseURL, "/") + "/v2/" + image + "/manifests/" + digest
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint, nil)
	if err != nil {
		return false, errs.Config
	}
	req.Header.Set("Accept", acceptManifest)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, transportError(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrBytes))

	switch resp.StatusCode {
	case http.StatusOK:
		if got := resp.Header.Get("Docker-Content-Digest"); got != "" && got != digest {
			return false, errs.LazaretteBadReply
		}
		return true, nil
	case http.StatusNotFound:
		return false, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return false, errs.LazaretteRefused
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return false, errs.LazaretteTimeout
	default:
		return false, errs.LazaretteBadReply
	}
}

func transportError(err error) error {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return errs.LazaretteTimeout
	}
	return errs.LazaretteUnreachable
}
