// Package example holds one made-up app and the made-up cluster objects
// that go with it. The end-to-end test and the demo both use it.
package example

import (
	"embed"
	"encoding/json"
)

//go:embed files
var files embed.FS

// App is the name of the example app.
const App = "example-app"

// Namespace is where the example app runs.
const Namespace = "cs-workers"

// File returns one of the example files.
func File(name string) []byte {
	data, err := files.ReadFile("files/" + name)
	if err != nil {
		panic(err)
	}
	return data
}

// Objects returns the JSON array in the named file, one element at a time.
func Objects(name string) []json.RawMessage {
	var out []json.RawMessage
	if err := json.Unmarshal(File(name), &out); err != nil {
		panic(err)
	}
	return out
}

// Digests returns the signed digests in digests.json: image -> digests.
func Digests() map[string][]string {
	var out map[string][]string
	if err := json.Unmarshal(File("digests.json"), &out); err != nil {
		panic(err)
	}
	return out
}
