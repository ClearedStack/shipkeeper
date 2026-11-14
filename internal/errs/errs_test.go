package errs

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/ClearedStack/shipkeeper/internal/golden"
)

// The whole list of errors is a saved example: adding, removing or
// rewording one must show up as a change to testdata/catalogue.txt.
func TestCatalogue(t *testing.T) {
	plain := regexp.MustCompile(`^[A-Za-z ,;.'"<>-]+$`)
	var out strings.Builder
	for _, e := range Catalogue() {
		if e.Code != Timeout && e.Code != Parse && e.Code != NeedsSubtool {
			t.Errorf("%q has a code that is not one of the three", e.Reason)
		}
		// No digits, slashes, colons or at-signs: nothing shaped like an
		// address, a path, a URL or a credential can be written here.
		if !plain.MatchString(e.Reason) {
			t.Errorf("%q has a character that is not allowed in an error", e.Reason)
		}
		out.WriteString(e.Error() + "\n")
	}
	golden.Equal(t, "testdata/catalogue.txt", []byte(out.String()))
}

func TestPublicHidesOutsideText(t *testing.T) {
	outside := fmt.Errorf("dial tcp 10.1.2.3:6443: connection refused")
	if got := Public(outside); got != Unexpected.Error() {
		t.Errorf("got %q", got)
	}
	if CodeOf(outside) != NeedsSubtool {
		t.Errorf("got %s", CodeOf(outside))
	}
	wrapped := fmt.Errorf("while applying: %w", ApplyConflict.WithFields([]string{".spec.replicas"}))
	if got := Public(wrapped); got != ApplyConflict.Error() {
		t.Errorf("got %q", got)
	}
	if !errors.Is(wrapped, ApplyConflict) || errors.Is(wrapped, KubeTimeout) {
		t.Error("errors.Is does not match catalogue entries")
	}
	if got := YAMLTab.At(7).Error(); got != "E_PARSE: app.yaml is indented with a tab (app.yaml line 7)" {
		t.Errorf("got %q", got)
	}
}
