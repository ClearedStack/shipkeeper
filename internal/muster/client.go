package muster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/ClearedStack/shipkeeper/internal/errs"
)

// Client sends health reports to Muster over cluster-private HTTP.
type Client struct {
	BaseURL   string       // e.g. http://muster.clearedstack.svc.cluster.local:8080
	Token     string       // bearer token used when TokenFile is empty
	TokenFile string       // ServiceAccount token path, read fresh on each call
	HTTP      *http.Client // must carry a timeout
}

type healthRequest struct {
	Contract string   `json:"contract"`
	Reports  []Report `json:"reports"`
}

type refusalWire struct {
	Error  string `json:"error"`
	Cause  string `json:"cause"`
	Reason string `json:"reason"`
}

// SendHealth calls POST /v1/health with contract "v1" and reports.
func (c *Client) SendHealth(ctx context.Context, reports []Report) (Summary, error) {
	var sum Summary
	if c.BaseURL == "" || c.HTTP == nil {
		return sum, errs.Config
	}
	token, err := c.bearerToken()
	if err != nil {
		return sum, err
	}
	list := reports
	if list == nil {
		list = []Report{}
	}
	payload, err := json.Marshal(healthRequest{
		Contract: ContractVersion,
		Reports:  list,
	})
	if err != nil {
		return sum, errs.Unexpected
	}
	if len(payload) > MaxBodyBytes {
		return sum, errs.MusterRefusedParse
	}

	url := strings.TrimRight(c.BaseURL, "/") + PathHealth
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return sum, errs.Config
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return sum, transportError(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return sum, transportError(err)
	}
	if len(body) > MaxBodyBytes {
		return sum, errs.MusterBadReply
	}

	if resp.StatusCode == http.StatusOK {
		if err := decodeSingleJSON(body, &sum); err != nil {
			return Summary{}, errs.MusterBadReply
		}
		return sum, nil
	}
	return Summary{}, decodeRefusal(resp.StatusCode, body)
}

// Healthz checks GET /healthz on Muster without presenting a bearer token.
func (c *Client) Healthz(ctx context.Context) error {
	if c.BaseURL == "" || c.HTTP == nil {
		return errs.Config
	}
	url := strings.TrimRight(c.BaseURL, "/") + PathHealthz
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errs.Config
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return transportError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return transportError(err)
	}
	if len(body) > MaxBodyBytes || resp.StatusCode != http.StatusOK {
		return errs.MusterBadReply
	}
	var status struct {
		Status string `json:"status"`
	}
	if err := decodeSingleJSON(body, &status); err != nil || status.Status != "ok" {
		return errs.MusterBadReply
	}
	return nil
}

func (c *Client) bearerToken() (string, error) {
	if c.TokenFile == "" {
		return strings.TrimSpace(c.Token), nil
	}
	raw, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return "", errs.Config
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", errs.Config
	}
	return tok, nil
}

func decodeSingleJSON(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

func decodeRefusal(status int, body []byte) error {
	var r refusalWire
	if err := decodeSingleJSON(body, &r); err == nil && r.Cause != "" && r.Reason != "" {
		code := errs.Code(r.Error)
		if code == errs.Timeout || code == errs.Parse || code == errs.NeedsSubtool {
			return &Refusal{
				Status: status,
				Code:   code,
				Cause:  r.Cause,
				Err:    causeError(r.Cause, code),
			}
		}
	}
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusTooManyRequests:
		return errs.MusterBusy
	default:
		return errs.MusterBadReply
	}
}

func transportError(err error) error {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return errs.MusterTimeout
	}
	return errs.MusterUnreachable
}
