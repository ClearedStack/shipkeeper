// Package golden compares test output with a saved example file.
//
// Run the tests with -update to rewrite the saved examples after a
// deliberate change in behaviour, then review the diff.
package golden

import (
	"flag"
	"os"
	"testing"
)

var update = flag.Bool("update", false, "rewrite saved examples instead of comparing")

// Equal fails the test when got differs from the saved example at path.
func Equal(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("saved example missing: %v", err)
	}
	if string(want) != string(got) {
		t.Errorf("%s: output differs from the saved example\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}
