// Package errs holds every error Shipkeeper can report.
//
// There are three public codes. Each error is a fixed entry in the catalogue
// below: a code and a sentence written in this file. No error text is ever
// built from outside data, so an error cannot name a machine, an address or
// a credential. The only variable parts are a line number in app.yaml and,
// for an apply conflict, the field paths that collided (kept out of the
// message and offered separately for the private log).
package errs

import (
	"errors"
	"strconv"
)

// Code is one of the three public error codes.
type Code string

const (
	Timeout      Code = "E_TIMEOUT"       // something outside took too long
	Parse        Code = "E_PARSE"         // the data was broken
	NeedsSubtool Code = "E_NEEDS_SUBTOOL" // the data was valid and unfamiliar
)

// Error is one catalogue entry, optionally pinned to a line of app.yaml.
type Error struct {
	Code   Code
	Reason string
	Line   int      // line in app.yaml, 0 when not applicable
	Fields []string // conflicting field paths; never part of Error()
}

func (e *Error) Error() string {
	s := string(e.Code) + ": " + e.Reason
	if e.Line > 0 {
		s += " (app.yaml line " + strconv.Itoa(e.Line) + ")"
	}
	return s
}

// Is reports whether target is the same catalogue entry, ignoring the line
// and fields.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code && t.Reason == e.Reason
}

// At returns a copy of the entry pinned to a line of app.yaml.
func (e *Error) At(line int) *Error {
	c := *e
	c.Line = line
	return &c
}

// WithFields returns a copy of the entry carrying conflicting field paths.
func (e *Error) WithFields(fields []string) *Error {
	c := *e
	c.Fields = fields
	return &c
}

// CodeOf returns the public code for err. An error that did not come from
// the catalogue is, by definition, something Shipkeeper has no rule for.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return NeedsSubtool
}

// Public returns the text that may be shown for err. Anything that is not a
// catalogue entry is replaced by Unexpected so that no outside text escapes.
func Public(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Error()
	}
	return Unexpected.Error()
}

var catalogue []*Error

func def(code Code, reason string) *Error {
	e := &Error{Code: code, Reason: reason}
	catalogue = append(catalogue, e)
	return e
}

// Catalogue returns every error Shipkeeper can report, in declaration order.
func Catalogue() []*Error { return append([]*Error(nil), catalogue...) }

var (
	// Reading app.yaml as text.
	YAMLNotUTF8     = def(Parse, "app.yaml holds bytes that are not valid text")
	YAMLEmpty       = def(Parse, "app.yaml is empty")
	YAMLTab         = def(Parse, "app.yaml is indented with a tab")
	YAMLIndent      = def(Parse, "app.yaml has a line at an indentation that fits nothing above it")
	YAMLNoKey       = def(Parse, "app.yaml has a line that is not a key followed by a colon")
	YAMLDuplicate   = def(Parse, "app.yaml gives the same key twice")
	YAMLQuote       = def(Parse, "app.yaml has a quoted value that is not closed properly")
	YAMLScalar      = def(Parse, "app.yaml has a value with a colon in it that is not quoted")
	YAMLUnsupported = def(NeedsSubtool, "app.yaml uses a YAML feature outside the small subset Shipkeeper reads")

	// Reading app.yaml as an app.
	ManifestShape     = def(Parse, "app.yaml has a field of the wrong shape")
	ManifestMissing   = def(Parse, "app.yaml is missing a required field")
	ManifestValue     = def(Parse, "app.yaml has a field value that is not in the accepted form")
	ManifestUnknown   = def(NeedsSubtool, "app.yaml has a field Shipkeeper does not know")
	ManifestAmbiguous = def(NeedsSubtool, "app.yaml has an unquoted word that YAML readers disagree about; quote it")
	ManifestNamespace = def(NeedsSubtool, "app.yaml names a namespace Shipkeeper does not manage")
	ManifestOtherApp  = def(Parse, "app.yaml describes a different app from the one asked for")

	// Plainforge, Lazarette and Muster.
	PlainforgeTimeout = def(Timeout, "Plainforge did not answer in time")
	PlainforgeNoApp   = def(NeedsSubtool, "Plainforge has no app.yaml for this app")
	LazaretteTimeout  = def(Timeout, "Lazarette did not answer in time")
	DigestMissing     = def(NeedsSubtool, "the digest in app.yaml is not among the signed digests Lazarette holds; nothing was applied")
	MusterTimeout     = def(Timeout, "Muster did not answer in time; the health report was not delivered")

	// The Kubernetes API.
	KubeTimeout     = def(Timeout, "the Kubernetes API did not answer in time")
	KubeUnreachable = def(Timeout, "the Kubernetes API could not be reached")
	KubeBadReply    = def(Parse, "the Kubernetes API sent a reply that could not be read")
	KubeRefused     = def(NeedsSubtool, "the Kubernetes API refused the request")
	KubeUnexpected  = def(NeedsSubtool, "the Kubernetes API answered in a way Shipkeeper has no rule for")
	ApplyConflict   = def(NeedsSubtool, "another field manager owns a field Shipkeeper sets, with a different value; nothing was overwritten")
	KubeKind        = def(NeedsSubtool, "Shipkeeper was asked to apply a kind of object it does not manage")

	// Building the health report.
	ObservedIncomplete = def(Parse, "the Deployment read back from the cluster is missing its replica count")
	ReportNotClean     = def(NeedsSubtool, "the health report still held a cluster detail after cleaning; it was not sent")

	// Configuration and anything else.
	Config     = def(Parse, "Shipkeeper was started with a setting that is missing or not in the accepted form")
	Unexpected = def(NeedsSubtool, "something happened that Shipkeeper has no rule for")
)
