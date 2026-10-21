// Command cloud-drift reports whether RavenDB Cloud moved under this operator
// since its simulator was last checked against it.
//
// Two checks. The public OpenAPI document is compared with the copy in
// .reference/swagger.json. And, when it is configured, the RavenDB Cloud source
// is compared between the pinned commit and the current one in the places this
// operator depends on. That source is private, so where it lives, which commit
// is pinned and which paths are watched come from the environment (repository
// secrets in CI), and in CI the check prints counts, never names.
//
//	CLOUD_SRC    a checkout of the RavenDB Cloud source
//	CLOUD_PIN    the commit the simulator was last checked against
//	CLOUD_WATCH  the paths to watch in it, separated by spaces
//	CLOUD_REF    what to compare the pin with; HEAD when unset
//
// Locally, keep those in .cloud-drift.env, which git ignores, and run
//
//	set -a; . ./.cloud-drift.env; set +a
//	git -C "$CLOUD_SRC" fetch origin
//	./hack/go.sh go run ./hack/cloud-drift
//
// hack/go.sh mounts CLOUD_SRC into its container. Once a change is reviewed and
// the simulator follows it, move CLOUD_PIN on, in .cloud-drift.env and in the
// repository secret.
//
// Exit status: 0 nothing moved, 1 something moved, 2 the check could not run.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"time"
)

const (
	publicOpenAPI = "https://api.cloud.ravendb.net/api/v1/swagger.json"
	vendored      = ".reference/swagger.json"
)

func main() {
	moved, err := run(os.Stdout, os.Getenv)
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, "cloud-drift:", err)
		os.Exit(2)
	case moved:
		os.Exit(1)
	}
}

func run(out io.Writer, getenv func(string) string) (moved bool, err error) {
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
	} else {
		moved = true
		fmt.Fprintf(out, "OpenAPI: the public document differs from %s in %d place(s):\n", vendored, len(changes))
		for _, c := range changes {
			fmt.Fprintln(out, "  "+c)
		}
	}

	src := sourceCheck{
		dir:   getenv("CLOUD_SRC"),
		pin:   getenv("CLOUD_PIN"),
		ref:   getenv("CLOUD_REF"),
		watch: strings.Fields(getenv("CLOUD_WATCH")),
		quiet: getenv("CI") == "true",
	}
	if !src.configured() {
		fmt.Fprintln(out, "Source: not configured (CLOUD_SRC, CLOUD_PIN, CLOUD_WATCH), skipped.")
		return moved, nil
	}
	sourceMoved, err := src.report(out)
	return moved || sourceMoved, err
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

// sourceCheck compares the private RavenDB Cloud source between the pinned
// commit and ref, in the watched paths only.
type sourceCheck struct {
	dir, pin, ref string
	watch         []string
	// quiet keeps file names and commit subjects out of the output, for CI
	// logs, which are as public as the repository.
	quiet bool
}

func (s sourceCheck) configured() bool {
	return s.dir != "" && s.pin != "" && len(s.watch) > 0
}

func (s sourceCheck) report(out io.Writer) (bool, error) {
	ref := s.ref
	if ref == "" {
		ref = "HEAD"
	}
	changed, err := s.git(append([]string{"diff", "--name-only", s.pin, ref, "--"}, s.watch...)...)
	if err != nil {
		return false, err
	}
	files := strings.Fields(changed)
	if len(files) == 0 {
		fmt.Fprintln(out, "Source: nothing watched changed since the pinned commit.")
		return false, nil
	}
	fmt.Fprintf(out, "Source: %d watched file(s) changed since the pinned commit.\n", len(files))
	if s.quiet {
		fmt.Fprintln(out, "  Run this locally to see which; CI logs do not name them.")
		return true, nil
	}
	for _, f := range files {
		fmt.Fprintln(out, "  "+f)
	}
	log, err := s.git(append([]string{"log", "--oneline", s.pin + ".." + ref, "--"}, s.watch...)...)
	if err != nil {
		return true, err
	}
	fmt.Fprint(out, "Commits:\n", indent(log))
	return true, nil
}

// git runs git in the source checkout. safe.directory is set because the
// checkout is usually mounted into a container whose user does not own it.
func (s sourceCheck) git(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", "safe.directory=*", "-C", s.dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if s.quiet {
			return "", fmt.Errorf("git %s failed", args[0])
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func indent(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}
