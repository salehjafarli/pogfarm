package render

import (
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/salehjafarli/pogfarm/"

// TestImports keeps the render core independent of the job loop and of
// pogfarm:
//
//   - no file of the package (tests excluded) imports internal/clipper/work,
//     internal/api (the API client and the job types) or internal/pogfarm;
//   - nothing the package depends on, directly or not, is
//     internal/clipper/work or under internal/pogfarm.
//
// internal/api itself is reachable through internal/config, which uses its
// Platform type; that is why it is checked as a direct import only.
func TestImports(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	direct := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			direct[path] = true
			if forbiddenDirect(path) {
				t.Errorf("%s imports %s", name, path)
			}
		}
	}
	if !direct[module+"internal/clipper/download"] {
		t.Fatal("the parse found no imports; is the test running in the package directory?")
	}

	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go command is not in PATH; only direct imports were checked")
	}
	out, err := exec.Command(goTool, "list", "-deps", "-f", "{{.ImportPath}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	deps := strings.Fields(string(out))
	if len(deps) < 10 {
		t.Fatalf("go list gave too few dependencies: %q", deps)
	}
	for _, dep := range deps {
		if dep == module+"internal/clipper/work" || strings.HasPrefix(dep, module+"internal/clipper/work/") ||
			dep == module+"internal/pogfarm" || strings.HasPrefix(dep, module+"internal/pogfarm/") {
			t.Errorf("render depends on %s", dep)
		}
	}
}

func forbiddenDirect(path string) bool {
	for _, p := range []string{"internal/clipper/work", "internal/api", "internal/pogfarm"} {
		if path == module+p || strings.HasPrefix(path, module+p+"/") {
			return true
		}
	}
	return false
}

func TestForbiddenDirect(t *testing.T) {
	for path, want := range map[string]bool{
		module + "internal/api":                     true,
		module + "internal/api/client":              true,
		module + "internal/clipper/work":            true,
		module + "internal/pogfarm/store":           true,
		module + "internal/apiary":                  false,
		module + "internal/clipper/download":        false,
		module + "internal/config":                  false,
		"github.com/other/internal/pogfarm/whatnot": false,
	} {
		if got := forbiddenDirect(path); got != want {
			t.Errorf("forbiddenDirect(%s) = %v, want %v", path, got, want)
		}
	}
}
