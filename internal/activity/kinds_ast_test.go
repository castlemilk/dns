package activity_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// activityImportPath is the package whose Kind type this test guards.
const activityImportPath = "github.com/castlemilk/dns/internal/activity"

// internalRoot is the tree to scan, relative to this package's directory.
const internalRoot = ".."

// kindsFile is the one file allowed to mint a Kind from a string: it declares
// the constants.
var kindsFile = filepath.Join(internalRoot, "activity", "kinds.go")

// TestNoFreeFormKindConversions is the real enforcement of spec2 §7.1's "a
// producer cannot pass a free-form string without an explicit conversion".
//
// golangci-lint's forbidigo rule in .golangci.yml is a secondary belt with two
// verified blind spots, which is why this test exists:
//
//   - forbidigo walks function bodies only, so a package-level
//     `var k = activity.Kind("made.up")` — exactly the shape someone reaching
//     for a missing constant would write — is never reported;
//   - the `source: activity\.Kind($|[^(])` exclusion that lets legitimate type
//     mentions through also swallows any conversion sharing a line with one,
//     e.g. `func f(s string) activity.Kind { return activity.Kind(s) }`.
//
// The AST has neither problem: a conversion is a CallExpr whose Fun is the
// selector, wherever it appears, and a type mention never is.
func TestNoFreeFormKindConversions(t *testing.T) {
	t.Parallel()

	scanned := 0
	err := filepath.WalkDir(internalRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path == internalRoot {
				return nil
			}
			if entry.Name() == "testdata" || strings.HasPrefix(entry.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			// Tests deliberately build an unknown kind to prove Record maps it
			// onto "other".
			return nil
		}
		if filepath.Clean(path) == filepath.Clean(kindsFile) {
			return nil
		}
		scanned++

		fileSet := token.NewFileSet()
		file, parseErr := parser.ParseFile(fileSet, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		for _, name := range kindConversions(file) {
			position := fileSet.Position(name.Pos())
			t.Errorf(
				"%s:%d: %s(…) converts a free-form string into an activity kind; "+
					"add a Kind constant to internal/activity/kinds.go instead (spec2 §7.1)",
				path, position.Line, name.Name)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", internalRoot, err)
	}
	if scanned == 0 {
		t.Fatalf("no Go files scanned under %s; the guard would pass vacuously", internalRoot)
	}
}

// kindConversions returns one entry per `<activity>.Kind(…)` or, inside package
// activity itself, per bare `Kind(…)` conversion in the file. The returned
// identifier is the conversion's own expression, so its position is the offence.
func kindConversions(file *ast.File) []*ast.Ident {
	local := activityImportName(file)
	inPackage := file.Name.Name == "activity"
	if local == "" && !inPackage {
		return nil
	}

	var found []*ast.Ident
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			// activity.Kind(x) — a qualified conversion. A method call on a
			// value that happens to be named `activity` would be a false
			// positive; no such variable exists, and renaming it is the fix.
			if fun.Sel.Name != "Kind" || local == "" {
				return true
			}
			ident, ok := fun.X.(*ast.Ident)
			if !ok || ident.Name != local {
				return true
			}
			found = append(found, fun.Sel)
		case *ast.Ident:
			// Kind(x) inside package activity itself.
			if inPackage && fun.Name == "Kind" {
				found = append(found, fun)
			}
		}
		return true
	})
	return found
}

// activityImportName returns the name internal/activity is bound to in this
// file ("" when it is not imported, "_" and "." are treated as not imported —
// neither can spell a conversion).
func activityImportName(file *ast.File) string {
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != activityImportPath {
			continue
		}
		if spec.Name == nil {
			return "activity"
		}
		if spec.Name.Name == "_" || spec.Name.Name == "." {
			return ""
		}
		return spec.Name.Name
	}
	return ""
}
