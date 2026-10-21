package arch_test

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/ravendb/ravendb-cloud-operator"

// rule forbids one import edge. Both sides match as path prefixes, so a rule
// keeps holding once either package grows a subpackage.
type rule struct {
	from      string // package directory, relative to the module root
	to        string // import path that directory may not reach for
	bindTests bool   // whether _test.go files under from are bound as well
	why       string // printed on failure, and says what to do instead
}

// The package graph is seven nodes and acyclic, so a full dependency matrix
// would cost more than it protects. These four edges are different: each is
// the shortest path to breaking an invariant CLAUDE.md states only in prose,
// and each is what a locally convenient edit reaches for first.
//
// All four already hold. The point is that breaking one fails `make test`
// rather than waiting for a reviewer to notice.
//
// When one fires, the fix is the alternative the message names. Relaxing a rule
// is an architecture decision, not a way to get to green.
var rules = []rule{
	{
		from:      "internal/cloudapi",
		to:        module + "/api/v1alpha1",
		bindTests: true,
		why: "internal/cloudapi knows only the request it is given. A rule needing the\n" +
			"region or instance-type lists, or an error message in CRD vocabulary, belongs\n" +
			"in internal/controller: the only place that can name the spec field to change.\n" +
			"A test here reaching for CRD types is the same signal one step early.",
	},
	{
		from: "internal/cloudsim",
		to:   module + "/internal/cloudapi",
		why: "The simulator keeps its own wire structs deliberately. Two independent\n" +
			"readings of the API are the only reason contract_test.go can catch a\n" +
			"misreading; sharing one set of types makes that test vacuous.\n" +
			"Test files here may import cloudapi, production code may not.",
	},
	{
		from: "internal/controller",
		to:   module + "/internal/cloudsim",
		why: "The simulator is test scaffolding. The controller depends on\n" +
			"cloudapi.Client, and tests point that client at the simulator over real\n" +
			"HTTP through spec.endpoint or RAVENDB_CLOUD_ENDPOINT.",
	},
	{
		from: "internal/export",
		to:   module + "/internal/cloudsim",
		why: "The export tool reads real clusters through cloudapi.Client, the same\n" +
			"door the operator uses. The simulator is for its tests only.",
	},
}

func TestForbiddenImports(t *testing.T) {
	root := moduleRoot(t)

	// A renamed package would leave its rule matching nothing and passing
	// forever, which is worse than no rule at all.
	for _, r := range rules {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(r.from))); err != nil {
			t.Errorf("rule source %q no longer exists, so the rule protects nothing: %v", r.from, err)
		}
	}

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		dir := path.Dir(rel)
		isTest := strings.HasSuffix(rel, "_test.go")

		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", rel, err)
		}

		for _, spec := range f.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			for _, r := range rules {
				if isTest && !r.bindTests {
					continue
				}
				if under(dir, r.from) && under(imp, r.to) {
					t.Errorf("%s imports %s\n\n%s\n", rel, imp, r.why)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}

// under reports whether p is prefix itself or sits below it. Comparing whole
// segments keeps internal/cloud from matching internal/cloudapi.
func under(p, prefix string) bool {
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}

// moduleRoot walks up to the go.mod, so the rules survive this file moving.
func moduleRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
