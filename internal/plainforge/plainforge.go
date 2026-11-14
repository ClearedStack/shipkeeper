// Package plainforge reads app.yaml manifests from bare Git repositories on
// Plainforge over Git's smart HTTP protocol, using the Go standard library
// only and no external git binary.
package plainforge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/registry"
)

const (
	defaultRef    = "refs/heads/main"
	maxRefsBytes  = 256 << 10
	maxPackBytes  = 4 << 20
	maxObjectSize = 1 << 20
	maxAppBytes   = 64 << 10
	maxObjects    = 4096
	maxCached     = 32
)

var (
	repoRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,126}[a-z0-9])?$`)
	oidRE  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Client reads app.yaml from Plainforge over cluster-private Git smart HTTP.
type Client struct {
	BaseURL string       // e.g. http://plainforge.clearedstack.svc.cluster.local:8080
	Ref     string       // branch ref; defaults to refs/heads/main
	HTTP    *http.Client // must carry a timeout

	mu    sync.Mutex
	cache map[string]cachedRepo // repo name -> cached files at commit OID
}

type cachedRepo struct {
	commit string
	files  map[string][]byte // app slug or "" -> app.yaml bytes
}

// AppManifest fetches app.yaml for app from its owning repository in
// Plainforge. Unchanged commits are served from an in-memory cache after a
// lightweight ref check.
func (c *Client) AppManifest(ctx context.Context, app string) ([]byte, error) {
	if c.BaseURL == "" || c.HTTP == nil {
		return nil, errs.Config
	}
	repo := app
	if comp, ok := registry.Lookup(app); ok {
		repo = comp.Repository
	}
	if !repoRE.MatchString(repo) || !repoRE.MatchString(app) {
		return nil, errs.Config
	}
	ref := c.Ref
	if ref == "" {
		ref = defaultRef
	}

	tip, err := c.fetchRef(ctx, repo, ref)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	hit, ok := c.cache[repo]
	c.mu.Unlock()
	if ok && hit.commit == tip {
		return pickManifest(hit.files, app)
	}

	files, err := c.fetchManifestsAt(ctx, repo, tip)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.cache == nil || len(c.cache) >= maxCached {
		c.cache = make(map[string]cachedRepo, maxCached)
	}
	c.cache[repo] = cachedRepo{commit: tip, files: files}
	c.mu.Unlock()

	return pickManifest(files, app)
}

func pickManifest(files map[string][]byte, app string) ([]byte, error) {
	if data, ok := files["apps/"+app+"/app.yaml"]; ok {
		out := make([]byte, len(data))
		copy(out, data)
		return out, nil
	}
	if data, ok := files["app.yaml"]; ok {
		out := make([]byte, len(data))
		copy(out, data)
		return out, nil
	}
	return nil, errs.PlainforgeNoApp
}

func (c *Client) fetchRef(ctx context.Context, repo, targetRef string) (string, error) {
	endpoint := strings.TrimRight(c.BaseURL, "/") + "/" + repo + ".git/info/refs?service=git-upload-pack"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", errs.Config
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", transportError(err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRefsBytes+1))
	if err != nil {
		return "", transportError(err)
	}
	if len(raw) > maxRefsBytes {
		return "", errs.PlainforgeBadReply
	}
	if err := checkStatus(resp.StatusCode); err != nil {
		return "", err
	}

	r := bytes.NewReader(raw)
	for {
		pkt, flush, eof, err := readPktLine(r)
		if err != nil {
			return "", errs.PlainforgeBadReply
		}
		if eof {
			break
		}
		if flush || bytes.HasPrefix(pkt, []byte("#")) {
			continue
		}
		line := pkt
		if i := bytes.IndexByte(line, 0); i >= 0 {
			line = line[:i]
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		oid, name, ok := strings.Cut(string(line), " ")
		if !ok || !oidRE.MatchString(oid) {
			return "", errs.PlainforgeBadReply
		}
		if name == targetRef {
			return oid, nil
		}
	}
	return "", errs.PlainforgeNoApp
}

func (c *Client) fetchManifestsAt(ctx context.Context, repo, commitOID string) (map[string][]byte, error) {
	var body bytes.Buffer
	writePktString(&body, fmt.Sprintf("want %s\n", commitOID))
	body.WriteString("0000")
	writePktString(&body, "done\n")

	endpoint := strings.TrimRight(c.BaseURL, "/") + "/" + repo + ".git/git-upload-pack"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return nil, errs.Config
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Accept", "application/x-git-upload-pack-result")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, transportError(err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxPackBytes+1))
	if err != nil {
		return nil, transportError(err)
	}
	if len(raw) > maxPackBytes {
		return nil, errs.PlainforgeBadReply
	}
	if err := checkStatus(resp.StatusCode); err != nil {
		return nil, err
	}

	packBytes, err := extractPack(raw)
	if err != nil {
		return nil, errs.PlainforgeBadReply
	}
	objs, err := unpack(packBytes)
	if err != nil {
		return nil, errs.PlainforgeBadReply
	}
	return extractAppFiles(objs, commitOID)
}

func checkStatus(status int) error {
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return errs.PlainforgeNoApp
	case http.StatusUnauthorized, http.StatusForbidden:
		return errs.PlainforgeRefused
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return errs.PlainforgeTimeout
	default:
		return errs.PlainforgeBadReply
	}
}

func transportError(err error) error {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return errs.PlainforgeTimeout
	}
	return errs.PlainforgeUnreachable
}

func writePktString(w *bytes.Buffer, s string) {
	fmt.Fprintf(w, "%04x%s", len(s)+4, s)
}

func readPktLine(r *bytes.Reader) (payload []byte, flush, eof bool, err error) {
	if r.Len() == 0 {
		return nil, false, true, nil
	}
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, false, false, err
	}
	n, err := strconv.ParseUint(string(hdr[:]), 16, 16)
	if err != nil {
		return nil, false, false, err
	}
	switch {
	case n == 0:
		return nil, true, false, nil
	case n < 4:
		return nil, false, false, errors.New("invalid pkt-line length")
	}
	buf := make([]byte, int(n)-4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, false, false, err
	}
	return buf, false, false, nil
}

// extractPack strips the leading NAK/ACK pkt-lines from a git-upload-pack
// response and returns the raw PACK stream (supporting both unframed PACK
// bytes and sideband-1 pkt-lines).
func extractPack(raw []byte) ([]byte, error) {
	r := bytes.NewReader(raw)
	for r.Len() > 0 {
		if r.Len() >= 4 {
			var peek [4]byte
			_, _ = r.Read(peek[:])
			_, _ = r.Seek(-4, io.SeekCurrent)
			if string(peek[:]) == "PACK" {
				rest := make([]byte, r.Len())
				_, _ = io.ReadFull(r, rest)
				return rest, nil
			}
		}
		pkt, flush, eof, err := readPktLine(r)
		if err != nil || eof {
			break
		}
		if flush || bytes.HasPrefix(pkt, []byte("NAK")) || bytes.HasPrefix(pkt, []byte("ACK ")) {
			continue
		}
		// Sideband framing: channel 1 carries pack data.
		if len(pkt) > 0 && pkt[0] == 1 {
			var out bytes.Buffer
			out.Write(pkt[1:])
			for {
				p, fl, end, err := readPktLine(r)
				if err != nil {
					return nil, err
				}
				if fl || end {
					return out.Bytes(), nil
				}
				if len(p) == 0 {
					continue
				}
				switch p[0] {
				case 1:
					out.Write(p[1:])
				case 2:
					// progress message, ignored
				default:
					return nil, errors.New("sideband error")
				}
			}
		}
		return nil, errors.New("unexpected git-upload-pack pkt-line")
	}
	return nil, errors.New("missing pack stream")
}
