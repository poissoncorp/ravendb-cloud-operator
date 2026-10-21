package main

import (
	"strings"
	"testing"
)

func TestOpenAPIChangesIgnoresFormattingAndNamesWhatMoved(t *testing.T) {
	pinned := []byte(`{"openapi":"3.0.4","paths":{"/a":{"get":{}},"/b":{"get":{}}},
		"components":{"schemas":{"Keep":{"type":"string"},"Gone":{"type":"string"}},"securitySchemes":{}}}`)

	same := []byte("{\n  \"components\": {\"securitySchemes\": {}, \"schemas\": {\"Gone\": {\"type\": \"string\"}, \"Keep\": {\"type\": \"string\"}}},\n" +
		"  \"paths\": {\"/b\": {\"get\": {}}, \"/a\": {\"get\": {}}},\n  \"openapi\": \"3.0.4\"\n}")
	if changes, err := openAPIChanges(pinned, same); err != nil || len(changes) != 0 {
		t.Fatalf("a reformatted copy reported %q, %v", changes, err)
	}

	moved := []byte(`{"openapi":"3.1.0","paths":{"/a":{"post":{}},"/c":{"get":{}}},
		"components":{"schemas":{"Keep":{"type":"string"},"New":{}},"securitySchemes":{}}}`)
	changes, err := openAPIChanges(pinned, moved)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"path added: /c", "path changed: /a", "path removed: /b",
		"schema added: New", "schema removed: Gone",
		"top-level changed: openapi",
	}
	if strings.Join(changes, "\n") != strings.Join(want, "\n") {
		t.Errorf("changes:\n%s\nwant:\n%s", strings.Join(changes, "\n"), strings.Join(want, "\n"))
	}
}
