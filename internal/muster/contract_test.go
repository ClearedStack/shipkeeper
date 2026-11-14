package muster_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/muster"
)

type savedExample struct {
	About   string        `json:"about"`
	Given   *exampleGiven `json:"given,omitempty"`
	Request exampleCall   `json:"request"`
	Expect  exampleExpect `json:"expect"`
}

type exampleGiven struct {
	Items     json.RawMessage `json:"items,omitempty"`
	Calls     []exampleCall   `json:"calls,omitempty"`
	Wait      string          `json:"wait,omitempty"`
	Condition string          `json:"condition,omitempty"`
}

type exampleCall struct {
	As          string          `json:"as,omitempty"`
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	ContentType *string         `json:"content_type,omitempty"`
	Body        json.RawMessage `json:"body,omitempty"`
	Raw         *string         `json:"raw,omitempty"`
}

type exampleExpect struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body,omitempty"`
	Stream *string         `json:"stream,omitempty"`
}

// nonShipkeeperPaths lists every contract v1 endpoint that belongs to workers
// or Conn rather than Shipkeeper. Any new endpoint in contract/v1/examples
// outside this map and Shipkeeper's own paths fails TestContractExamples.
var nonShipkeeperPaths = map[string]bool{
	"/v1/lease":         true,
	"/v1/heartbeat":     true,
	"/v1/complete":      true,
	"/v1/fail":          true,
	"/v1/needs_subtool": true,
	"/v1/release":       true,
	"/v1/items":         true,
	"/v1/edits":         true,
	"/v1/feed":          true,
}

// retryableCauses lists every refusal cause in CONTRACT.md and whether
// Shipkeeper must keep its pending health batch and retry later.
var retryableCauses = map[string]bool{
	"store_slow":       true,
	"busy":             true,
	"rate_limited":     true,
	"not_json":         false,
	"unknown_field":    false,
	"missing_field":    false,
	"bad_field":        false,
	"no_credential":    false,
	"not_your_call":    false,
	"no_such_call":     false,
	"wrong_method":     false,
	"too_large":        false,
	"wrong_type":       false,
	"lease_not_held":   false,
	"move_not_allowed": false,
	"duplicate":        false,
	"unknown_contract": false,
	"unknown_value":    false,
	"bad_record":       false,
	"unexpected":       false,
}

func compactJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("invalid JSON %s: %v", raw, err)
	}
	return buf.String()
}

// TestContractExamples replays every saved example in contract/v1/examples/.
func TestContractExamples(t *testing.T) {
	paths, err := filepath.Glob("testdata/examples/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no saved contract examples found (%v)", err)
	}
	if len(paths) < 52 {
		t.Fatalf("found %d examples, expected at least 52", len(paths))
	}

	catalogueSet := map[string]bool{}
	for _, entry := range errs.Catalogue() {
		catalogueSet[entry.Error()] = true
	}

	var shipkeeperCalls, refusalCalls, otherCalls, healthzCalls int

	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var ex savedExample
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&ex); err != nil {
				t.Fatalf("decode %s: %v", name, err)
			}
			if ex.About == "" {
				t.Fatal("example is missing about")
			}

			// 1. Every refusal example is replayed against Client.SendHealth to
			// verify exact status, cause, public error code, catalogue safety,
			// and retry classification.
			if ex.Expect.Status >= 400 {
				refusalCalls++
				replayRefusalAgainstClient(t, ex, catalogueSet)
			}

			// 2. Dispatch by endpoint to exercise Shipkeeper's own calls end to
			// end (both Client and in-memory contract stand-in) and verify
			// caller boundaries on every other call.
			switch ex.Request.Path {
			case muster.PathHealth:
				if ex.Request.As == "shipkeeper" && ex.Request.Method == http.MethodPost &&
					ex.Request.Raw == nil && ex.Request.ContentType == nil {
					shipkeeperCalls++
					replayHealthCall(t, ex)
				}
			case muster.PathHealthz:
				healthzCalls++
				replayHealthzCall(t, ex)
			default:
				if ex.Expect.Status < 400 {
					otherCalls++
					if !nonShipkeeperPaths[ex.Request.Path] {
						t.Fatalf("unhandled contract path %q", ex.Request.Path)
					}
					if ex.Request.As == "shipkeeper" {
						t.Fatalf("shipkeeper must never call %s", ex.Request.Path)
					}
					replayForbiddenAsShipkeeper(t, ex)
				}
			}
		})
	}

	if shipkeeperCalls != 5 {
		t.Fatalf("replayed %d shipkeeper /v1/health examples, want 5", shipkeeperCalls)
	}
	if healthzCalls != 1 {
		t.Fatalf("replayed %d /healthz examples, want 1", healthzCalls)
	}
	if refusalCalls != 28 {
		t.Fatalf("replayed %d refusal examples, want 28", refusalCalls)
	}
	if otherCalls != 20 {
		t.Fatalf("checked %d worker/conn examples, want 20", otherCalls)
	}
}

func replayRefusalAgainstClient(t *testing.T, ex savedExample, catalogueSet map[string]bool) {
	t.Helper()
	var want struct {
		Error  errs.Code `json:"error"`
		Cause  string    `json:"cause"`
		Reason string    `json:"reason"`
	}
	if err := json.Unmarshal(ex.Expect.Body, &want); err != nil {
		t.Fatalf("unmarshal refusal body: %v", err)
	}
	wantRetry, known := retryableCauses[want.Cause]
	if !known {
		t.Fatalf("refusal cause %q is not in retryableCauses table", want.Cause)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ex.Expect.Status)
		_, _ = w.Write(ex.Expect.Body)
	}))
	defer srv.Close()

	client := &muster.Client{
		BaseURL: srv.URL,
		Token:   "token-shipkeeper",
		HTTP:    &http.Client{Timeout: 2 * time.Second},
	}
	_, err := client.SendHealth(context.Background(), []muster.Report{
		{Component: "shipkeeper", State: muster.Healthy},
	})
	if err == nil {
		t.Fatal("expected refusal error, got nil")
	}
	var ref *muster.Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("expected *muster.Refusal, got %T (%v)", err, err)
	}
	if ref.Status != ex.Expect.Status || ref.Cause != want.Cause || ref.Code != want.Error {
		t.Fatalf("got refusal (%d, %s, %s), want (%d, %s, %s)",
			ref.Status, ref.Cause, ref.Code, ex.Expect.Status, want.Cause, want.Error)
	}
	if got := errs.CodeOf(err); got != want.Error {
		t.Fatalf("CodeOf: got %s, want %s", got, want.Error)
	}
	if got := muster.Retryable(err); got != wantRetry {
		t.Fatalf("Retryable(%s): got %v, want %v", want.Cause, got, wantRetry)
	}
	pub := errs.Public(err)
	if !catalogueSet[pub] {
		t.Fatalf("Public(err) %q is not in Shipkeeper's error catalogue", pub)
	}
}

func replayHealthCall(t *testing.T, ex savedExample) {
	t.Helper()
	var reqBody struct {
		Contract string          `json:"contract"`
		Reports  []muster.Report `json:"reports"`
	}
	if err := json.Unmarshal(ex.Request.Body, &reqBody); err != nil {
		t.Fatalf("decode health request: %v", err)
	}

	// Part A: replay against stub HTTP server to check exact wire bytes from Client.
	var gotMethod, gotPath, gotAuth, gotCT string
	var gotWire []byte
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotWire, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ex.Expect.Status)
		_, _ = w.Write(ex.Expect.Body)
	}))
	defer stub.Close()

	client := &muster.Client{
		BaseURL: stub.URL,
		Token:   "token-shipkeeper",
		HTTP:    &http.Client{Timeout: 2 * time.Second},
	}
	sum, err := client.SendHealth(context.Background(), reqBody.Reports)
	if gotMethod != http.MethodPost || gotPath != muster.PathHealth {
		t.Fatalf("wire call %s %s, want POST %s", gotMethod, gotPath, muster.PathHealth)
	}
	if gotAuth != "Bearer token-shipkeeper" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Fatalf("Content-Type = %q", gotCT)
	}
	if compactJSON(t, gotWire) != compactJSON(t, ex.Request.Body) {
		t.Fatalf("wire body mismatch:\n got: %s\nwant: %s", compactJSON(t, gotWire), compactJSON(t, ex.Request.Body))
	}

	if ex.Expect.Status == http.StatusOK {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		gotSum, _ := json.Marshal(sum)
		if compactJSON(t, gotSum) != compactJSON(t, ex.Expect.Body) {
			t.Fatalf("decoded summary %s != %s", compactJSON(t, gotSum), compactJSON(t, ex.Expect.Body))
		}
	} else if err == nil {
		t.Fatalf("expected error for status %d", ex.Expect.Status)
	}

	// Part B: replay against Shipkeeper's in-memory Muster stand-in server,
	// seeded from ex.Given.
	mem := seedMemoryFromGiven(t, ex.Given)
	standin := httptest.NewServer(mem)
	defer standin.Close()

	liveClient := &muster.Client{
		BaseURL: standin.URL,
		Token:   "token-shipkeeper",
		HTTP:    &http.Client{Timeout: 2 * time.Second},
	}
	sum2, err2 := liveClient.SendHealth(context.Background(), reqBody.Reports)
	if ex.Expect.Status == http.StatusOK {
		if err2 != nil {
			t.Fatalf("stand-in error: %v", err2)
		}
		gotSum2, _ := json.Marshal(sum2)
		if compactJSON(t, gotSum2) != compactJSON(t, ex.Expect.Body) {
			t.Fatalf("stand-in summary %s != %s", compactJSON(t, gotSum2), compactJSON(t, ex.Expect.Body))
		}
	} else {
		var ref *muster.Refusal
		if !errors.As(err2, &ref) || ref.Status != ex.Expect.Status {
			t.Fatalf("stand-in refusal: got %v, want status %d", err2, ex.Expect.Status)
		}
	}
}

func seedMemoryFromGiven(t *testing.T, g *exampleGiven) *muster.Memory {
	t.Helper()
	mem := &muster.Memory{
		PausedBy:       map[string]int{},
		BuildingByRepo: map[string]int{},
	}
	if g == nil {
		return mem
	}
	var editRepos []string
	for _, c := range g.Calls {
		switch c.Path {
		case "/v1/needs_subtool":
			mem.PausedBy[c.As]++
		case "/v1/edits":
			var body struct {
				Target string `json:"target"`
			}
			_ = json.Unmarshal(c.Body, &body)
			if strings.HasPrefix(body.Target, "catalog.") {
				editRepos = append(editRepos, "clearedstack.com")
			}
		case "/v1/complete":
			var body struct {
				State string `json:"state"`
			}
			_ = json.Unmarshal(c.Body, &body)
			if c.As == "bellbuoy" && body.State == "building" && len(editRepos) > 0 {
				mem.BuildingByRepo[editRepos[0]]++
				editRepos = editRepos[1:]
			}
		case muster.PathHealth:
			var body struct {
				Reports []muster.Report `json:"reports"`
			}
			if err := json.Unmarshal(c.Body, &body); err != nil {
				t.Fatal(err)
			}
			if _, err := mem.SendHealth(context.Background(), body.Reports); err != nil {
				t.Fatalf("given health call failed: %v", err)
			}
		}
	}
	return mem
}

func replayHealthzCall(t *testing.T, ex savedExample) {
	t.Helper()
	var gotAuth string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodGet || r.URL.Path != muster.PathHealthz {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ex.Expect.Status)
		_, _ = w.Write(ex.Expect.Body)
	}))
	defer stub.Close()

	c := &muster.Client{BaseURL: stub.URL, Token: "token-shipkeeper", HTTP: &http.Client{Timeout: 2 * time.Second}}
	if err := c.Healthz(context.Background()); err != nil {
		t.Fatalf("Healthz against stub: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("Healthz must not send Authorization header, sent %q", gotAuth)
	}

	standin := httptest.NewServer(&muster.Memory{})
	defer standin.Close()
	c.BaseURL = standin.URL
	if err := c.Healthz(context.Background()); err != nil {
		t.Fatalf("Healthz against stand-in: %v", err)
	}
}

func replayForbiddenAsShipkeeper(t *testing.T, ex savedExample) {
	t.Helper()
	standin := httptest.NewServer(&muster.Memory{})
	defer standin.Close()

	req, err := http.NewRequest(ex.Request.Method, standin.URL+ex.Request.Path, bytes.NewReader(ex.Request.Body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer token-shipkeeper")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("shipkeeper calling %s %s: got %d, want 403", ex.Request.Method, ex.Request.Path, resp.StatusCode)
	}
}
