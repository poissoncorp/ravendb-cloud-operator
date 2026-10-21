// Command cloud-drift reports whether RavenDB Cloud's published OpenAPI
// document moved since .reference/swagger.json, the copy the client and the
// simulator were last checked against.
//
//	./hack/go.sh go run ./hack/cloud-drift
//
// Exit status: 0 nothing moved, 1 it moved, 2 the check could not run.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"slices"
	"time"
)

const (
	publicOpenAPI = "https://api.cloud.ravendb.net/api/v1/swagger.json"
	vendored      = ".reference/swagger.json"
)

func main() {
	moved, err := run(os.Stdout)
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, "cloud-drift:", err)
		os.Exit(2)
	case moved:
		os.Exit(1)
	}
}

func run(out io.Writer) (moved bool, err error) {
	live, err := fetch(publicOpenAPI)
	if err != nil {
		return false, err
	}
	pinned, err := os.ReadFile(vendored)
	if err != nil {
		return false, err
	}
	changes, err := openAPIChanges(pinned, live)
	if err != nil {
		return false, err
	}
	if len(changes) == 0 {
		fmt.Fprintln(out, "OpenAPI: the public document matches "+vendored+".")
		return false, nil
	}
	fmt.Fprintf(out, "OpenAPI: the public document differs from %s in %d place(s):\n", vendored, len(changes))
	for _, c := range changes {
		fmt.Fprintln(out, "  "+c)
	}
	return true, nil
}

func fetch(url string) ([]byte, error) {
	client := &http.Client{Timeout: time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// openAPIChanges compares two OpenAPI documents by meaning rather than bytes,
// and names what changed: each path and schema on its own, everything else by
// its top-level key.
func openAPIChanges(pinned, live []byte) ([]string, error) {
	var a, b map[string]any
	if err := json.Unmarshal(pinned, &a); err != nil {
		return nil, fmt.Errorf("reading %s: %w", vendored, err)
	}
	if err := json.Unmarshal(live, &b); err != nil {
		return nil, fmt.Errorf("reading the public document: %w", err)
	}

	var changes []string
	changes = append(changes, keyedChanges("path", child(a, "paths"), child(b, "paths"))...)
	changes = append(changes, keyedChanges("schema", child(child(a, "components"), "schemas"),
		child(child(b, "components"), "schemas"))...)
	delete(a, "paths")
	delete(b, "paths")
	if components := child(a, "components"); components != nil {
		delete(components, "schemas")
	}
	if components := child(b, "components"); components != nil {
		delete(components, "schemas")
	}
	changes = append(changes, keyedChanges("top-level", a, b)...)
	return changes, nil
}

// keyedChanges lists the keys added, removed or changed between two objects.
func keyedChanges(kind string, before, after map[string]any) []string {
	var changes []string
	for key, old := range before {
		updated, kept := after[key]
		switch {
		case !kept:
			changes = append(changes, fmt.Sprintf("%s removed: %s", kind, key))
		case !reflect.DeepEqual(old, updated):
			changes = append(changes, fmt.Sprintf("%s changed: %s", kind, key))
		}
	}
	for key := range after {
		if _, existed := before[key]; !existed {
			changes = append(changes, fmt.Sprintf("%s added: %s", kind, key))
		}
	}
	slices.Sort(changes)
	return changes
}

func child(parent map[string]any, key string) map[string]any {
	m, _ := parent[key].(map[string]any)
	return m
}
