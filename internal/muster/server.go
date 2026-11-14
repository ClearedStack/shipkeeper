package muster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/registry"
)

var (
	validStates = map[State]bool{
		Healthy:  true,
		Idle:     true,
		Degraded: true,
		Paused:   true,
	}
	digestForm = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Memory is an in-memory stand-in for Muster's contract v1 health endpoint.
// It implements both Sender (in-process) and http.Handler (over HTTP) using
// the exact rules of CONTRACT.md, including digest-triggered worker replay
// counts and building-to-live edit proposal counts.
type Memory struct {
	mu sync.Mutex

	// Down makes POST /v1/health fail as 503 store_slow (Logbook unavailable).
	Down bool

	// Tokens maps bearer token -> caller slug for HTTP requests.
	// When nil, token "token-<slug>" identifies <slug>.
	Tokens map[string]string

	// Batches holds every accepted batch in arrival order.
	Batches [][]Report

	// Digests holds the last digest recorded for each component slug.
	Digests map[string]string

	// PausedBy holds the number of paused items per worker slug waiting to be
	// replayed when that worker's digest changes.
	PausedBy map[string]int

	// BuildingByRepo holds the number of building edit proposals per
	// repository waiting to move to live when a component in that repository
	// reports a changed digest.
	BuildingByRepo map[string]int
}

type problem struct {
	status int
	code   errs.Code
	cause  string
	reason string
}

var (
	probNotJSON         = &problem{http.StatusBadRequest, errs.Parse, "not_json", "the request body is not valid JSON"}
	probUnknownField    = &problem{http.StatusBadRequest, errs.Parse, "unknown_field", "the request body has a field this call does not take"}
	probMissingField    = &problem{http.StatusBadRequest, errs.Parse, "missing_field", "a required field is missing or empty"}
	probBadField        = &problem{http.StatusBadRequest, errs.Parse, "bad_field", "a field has a value that is not allowed"}
	probNoCredential    = &problem{http.StatusUnauthorized, errs.Parse, "no_credential", "no valid bearer token was presented"}
	probNotYourCall     = &problem{http.StatusForbidden, errs.Parse, "not_your_call", "this caller may not make that call"}
	probNoSuchCall      = &problem{http.StatusNotFound, errs.Parse, "no_such_call", "there is no such call"}
	probWrongMethod     = &problem{http.StatusMethodNotAllowed, errs.Parse, "wrong_method", "that HTTP method is not allowed here"}
	probTooLarge        = &problem{http.StatusRequestEntityTooLarge, errs.Parse, "too_large", "the request body is over the limit"}
	probWrongType       = &problem{http.StatusUnsupportedMediaType, errs.Parse, "wrong_type", "the content type must be application/json"}
	probDuplicate       = &problem{http.StatusConflict, errs.Parse, "duplicate", "the same component is reported twice in one batch"}
	probUnknownContract = &problem{http.StatusUnprocessableEntity, errs.NeedsSubtool, "unknown_contract", "this contract version is not known"}
	probUnknownHealth   = &problem{http.StatusUnprocessableEntity, errs.NeedsSubtool, "unknown_value", "this health state is not known"}
	probStoreSlow       = &problem{http.StatusServiceUnavailable, errs.Timeout, "store_slow", "logbook did not answer in time"}
)

var otherContractEndpoints = map[string]string{
	"/v1/lease":         http.MethodPost,
	"/v1/heartbeat":     http.MethodPost,
	"/v1/complete":      http.MethodPost,
	"/v1/fail":          http.MethodPost,
	"/v1/needs_subtool": http.MethodPost,
	"/v1/release":       http.MethodPost,
	"/v1/items":         http.MethodPost,
	"/v1/edits":         http.MethodPost,
	"/v1/feed":          http.MethodGet,
}

// SendHealth validates and records reports in memory under CONTRACT.md rules.
func (m *Memory) SendHealth(_ context.Context, reports []Report) (Summary, error) {
	sum, p := m.receive(reports)
	if p != nil {
		return Summary{}, &Refusal{
			Status: p.status,
			Code:   p.code,
			Cause:  p.cause,
			Err:    causeError(p.cause, p.code),
		}
	}
	return sum, nil
}

// LastReports returns a copy of the most recently accepted batch, if any.
func (m *Memory) LastReports() []Report {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.Batches) == 0 {
		return nil
	}
	last := m.Batches[len(m.Batches)-1]
	out := make([]Report, len(last))
	copy(out, last)
	return out
}

func (m *Memory) receive(reports []Report) (Summary, *problem) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Down {
		return Summary{}, probStoreSlow
	}
	if len(reports) == 0 {
		return Summary{}, probMissingField
	}
	if len(reports) > MaxReports {
		return Summary{}, probBadField
	}
	seen := make(map[string]bool, len(reports))
	for _, r := range reports {
		if r.Component == "" || r.State == "" {
			return Summary{}, probMissingField
		}
		if _, ok := registry.Lookup(r.Component); !ok {
			return Summary{}, probBadField
		}
		if seen[r.Component] {
			return Summary{}, probDuplicate
		}
		seen[r.Component] = true
		switch r.Code {
		case "", errs.Timeout, errs.Parse, errs.NeedsSubtool:
		default:
			return Summary{}, probBadField
		}
		if r.Digest != "" && !digestForm.MatchString(r.Digest) {
			return Summary{}, probBadField
		}
	}
	for _, r := range reports {
		if !validStates[r.State] {
			return Summary{}, probUnknownHealth
		}
	}

	if m.Digests == nil {
		m.Digests = map[string]string{}
	}
	sum := Summary{Received: len(reports)}
	for _, r := range reports {
		if r.Digest != "" {
			prev := m.Digests[r.Component]
			if prev != "" && prev != r.Digest {
				if registry.IsWorker(r.Component) && m.PausedBy != nil {
					sum.Replayed += m.PausedBy[r.Component]
					m.PausedBy[r.Component] = 0
				}
				if comp, ok := registry.Lookup(r.Component); ok && m.BuildingByRepo != nil {
					sum.Live += m.BuildingByRepo[comp.Repository]
					m.BuildingByRepo[comp.Repository] = 0
				}
			}
			m.Digests[r.Component] = r.Digest
		}
	}
	cp := make([]Report, len(reports))
	copy(cp, reports)
	m.Batches = append(m.Batches, cp)
	return sum, nil
}

// ServeHTTP serves GET /healthz, POST /v1/health, and contract refusals for
// other paths and callers.
func (m *Memory) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == PathHealthz {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeProblem(w, probWrongMethod)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Status string `json:"status"`
		}{Status: "ok"})
		return
	}

	wantMethod := ""
	if r.URL.Path == PathHealth {
		wantMethod = http.MethodPost
	} else if om, ok := otherContractEndpoints[r.URL.Path]; ok {
		wantMethod = om
	} else {
		writeProblem(w, probNoSuchCall)
		return
	}
	if r.Method != wantMethod {
		w.Header().Set("Allow", wantMethod)
		writeProblem(w, probWrongMethod)
		return
	}

	caller, ok := m.authenticate(r)
	if !ok {
		writeProblem(w, probNoCredential)
		return
	}
	if r.URL.Path != PathHealth || caller != registry.Shipkeeper {
		writeProblem(w, probNotYourCall)
		return
	}

	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != "application/json" {
		writeProblem(w, probWrongType)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeProblem(w, probTooLarge)
			return
		}
		writeProblem(w, probNotJSON)
		return
	}
	var req healthRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.As(err, &typeErr):
			writeProblem(w, probBadField)
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			writeProblem(w, probUnknownField)
		default:
			writeProblem(w, probNotJSON)
		}
		return
	}
	if _, err := dec.Token(); err != io.EOF {
		writeProblem(w, probNotJSON)
		return
	}
	switch req.Contract {
	case ContractVersion:
	case "":
		writeProblem(w, probMissingField)
		return
	default:
		writeProblem(w, probUnknownContract)
		return
	}

	sum, p := m.receive(req.Reports)
	if p != nil {
		writeProblem(w, p)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

func (m *Memory) authenticate(r *http.Request) (string, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return "", false
	}
	m.mu.Lock()
	tokens := m.Tokens
	m.mu.Unlock()
	if tokens != nil {
		slug, ok := tokens[tok]
		return slug, ok
	}
	slug, ok := strings.CutPrefix(tok, "token-")
	if !ok {
		return "", false
	}
	if _, exists := registry.Lookup(slug); !exists {
		return "", false
	}
	return slug, true
}

func writeProblem(w http.ResponseWriter, p *problem) {
	writeJSON(w, p.status, refusalWire{
		Error:  string(p.code),
		Cause:  p.cause,
		Reason: p.reason,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
