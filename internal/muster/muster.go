// Package muster speaks Muster's contract v1 from Shipkeeper: POST /v1/health
// with cleaned component health reports, and GET /healthz.
package muster

import (
	"context"
	"errors"

	"github.com/ClearedStack/shipkeeper/internal/errs"
)

// Contract v1 constants.
const (
	ContractVersion = "v1"
	MaxReports      = 64
	MaxBodyBytes    = 64 << 10
	PathHealth      = "/v1/health"
	PathHealthz     = "/healthz"
)

// State is one of the four component health states in CONTRACT.md.
type State string

const (
	Healthy  State = "healthy"
	Idle     State = "idle"
	Degraded State = "degraded"
	Paused   State = "paused"
)

// Report is one entry in the reports list of POST /v1/health.
// Field order matches contract/v1/examples/health*.json.
type Report struct {
	Component string    `json:"component"`
	State     State     `json:"state"`
	Code      errs.Code `json:"code,omitempty"`
	Digest    string    `json:"digest,omitempty"`
}

// Summary is Muster's 200 answer to POST /v1/health.
type Summary struct {
	Received int `json:"received"`
	Replayed int `json:"replayed"`
	Live     int `json:"live"`
}

// Sender delivers a batch of component health reports to Muster.
type Sender interface {
	SendHealth(ctx context.Context, reports []Report) (Summary, error)
}

// Refusal describes a structured contract v1 refusal returned by Muster.
// Its error text comes from Shipkeeper's own catalogue so no outside string
// ever leaves the client.
type Refusal struct {
	Status int
	Code   errs.Code
	Cause  string
	Err    *errs.Error
}

func (r *Refusal) Error() string { return r.Err.Error() }
func (r *Refusal) Unwrap() error { return r.Err }

// Retryable reports whether err is a transient Muster or Logbook condition
// for which Shipkeeper should keep its pending health reports and try again
// on a later pass.
func Retryable(err error) bool {
	return errors.Is(err, errs.MusterTimeout) ||
		errors.Is(err, errs.MusterUnreachable) ||
		errors.Is(err, errs.MusterBusy)
}

// causeError maps a contract v1 refusal cause and code to Shipkeeper's error
// catalogue while preserving the exact public code (E_TIMEOUT, E_PARSE, or
// E_NEEDS_SUBTOOL).
func causeError(cause string, code errs.Code) *errs.Error {
	switch cause {
	case "no_credential":
		return errs.MusterNoCredential
	case "not_your_call":
		return errs.MusterNotAllowed
	case "duplicate":
		return errs.MusterDuplicate
	case "unknown_contract":
		return errs.MusterUnknownContract
	case "unknown_value":
		return errs.MusterUnknownValue
	case "store_slow", "busy", "rate_limited":
		return errs.MusterBusy
	case "not_json", "unknown_field", "missing_field", "bad_field",
		"no_such_call", "wrong_method", "too_large", "wrong_type",
		"lease_not_held", "move_not_allowed", "bad_record", "unexpected":
		if code == errs.Timeout {
			return errs.MusterBusy
		}
		if code == errs.NeedsSubtool {
			return errs.MusterUnknownValue
		}
		return errs.MusterRefusedParse
	}
	switch code {
	case errs.Timeout:
		return errs.MusterBusy
	case errs.NeedsSubtool:
		return errs.MusterUnknownValue
	default:
		return errs.MusterRefusedParse
	}
}
