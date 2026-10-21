package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
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

// TestTheSourceCheckNamesNothingInCI: CI logs are as public as the
// repository, and the source is not, so in CI only the count is printed.
func TestTheSourceCheckNamesNothingInCI(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("init", "-q")
	write("api/rules.txt", "v1")
	write("billing/invoice.txt", "v1")
	git("add", ".")
	git("commit", "-q", "-m", "start")
	pin := git("rev-parse", "HEAD")

	write("billing/invoice.txt", "v2")
	git("commit", "-q", "-am", "billing only")
	check := sourceCheck{dir: dir, pin: pin, watch: []string{"api"}}
	var out bytes.Buffer
	if moved, err := check.report(&out); err != nil || moved {
		t.Fatalf("a change outside the watched paths reported moved=%v, %v:\n%s", moved, err, out.String())
	}

	write("api/rules.txt", "v2")
	git("commit", "-q", "-am", "validator rule")

	out.Reset()
	if moved, err := check.report(&out); err != nil || !moved || !strings.Contains(out.String(), "api/rules.txt") {
		t.Fatalf("locally the changed file should be named: moved=%v, %v:\n%s", moved, err, out.String())
	}

	check.quiet = true
	out.Reset()
	if moved, err := check.report(&out); err != nil || !moved {
		t.Fatalf("moved=%v, %v", moved, err)
	}
	for _, secret := range []string{"rules", "api/", "validator rule"} {
		if strings.Contains(out.String(), secret) {
			t.Errorf("the CI output names %q:\n%s", secret, out.String())
		}
	}
}
