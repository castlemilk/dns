package telemetry

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// generatedRoot is the generated Go tree, relative to this package's directory.
const generatedRoot = "../../gen/go"

// clientOnlyPackages are generated clients this process calls out to. Their
// procedures are never served here, so they must NOT appear in httpRoutes: a
// route label for a path the mux does not answer would be a lie in the metric.
var clientOnlyPackages = []string{"deephost"}

// procedureConstants walks the generated Connect code and returns every
// procedure string literal, keyed by the file it came from.
func procedureConstants(t *testing.T) map[string]string {
	t.Helper()
	found := make(map[string]string)
	err := filepath.WalkDir(generatedRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			for _, skip := range clientOnlyPackages {
				if entry.Name() == skip {
					return fs.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".connect.go") {
			return nil
		}
		fileSet := token.NewFileSet()
		file, parseErr := parser.ParseFile(fileSet, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.CONST {
				continue
			}
			for _, spec := range general.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for index, name := range value.Names {
					if !strings.HasSuffix(name.Name, "Procedure") || index >= len(value.Values) {
						continue
					}
					literal, ok := value.Values[index].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					procedure, unquoteErr := strconv.Unquote(literal.Value)
					if unquoteErr != nil {
						return unquoteErr
					}
					found[procedure] = path
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", generatedRoot, err)
	}
	if len(found) == 0 {
		t.Fatalf("no generated procedure constants found under %s", generatedRoot)
	}
	return found
}

func TestEveryServedProcedureHasABoundedRouteAndMethod(t *testing.T) {
	for procedure, path := range procedureConstants(t) {
		if _, ok := httpRoutes[procedure]; !ok {
			t.Errorf("procedure %s (%s) is not in httpRoutes; add it to telemetry.procedures", procedure, path)
		}
		service, method := rpcTarget(procedure)
		if service == "unknown" {
			t.Errorf("procedure %s (%s) has no bounded rpc.service", procedure, path)
			continue
		}
		if method == "unknown" {
			t.Errorf("procedure %s (%s) has no bounded rpc.method", procedure, path)
		}
	}
}

func TestProceduresListMatchesTheGeneratedCode(t *testing.T) {
	generated := procedureConstants(t)
	for _, procedure := range procedures {
		if _, ok := generated[procedure]; !ok {
			t.Errorf("telemetry.procedures lists %s, which no generated Connect file declares", procedure)
		}
	}
	if len(procedures) != len(generated) {
		t.Errorf("telemetry.procedures has %d entries, the generated code declares %d", len(procedures), len(generated))
	}
}

func TestClientOnlyProceduresAreNotRoutes(t *testing.T) {
	// The DeepHost client's procedures must not be labelled as routes of this
	// server. Parse them explicitly rather than trusting the skip above.
	root := filepath.Join(generatedRoot, "deephost")
	var seen int
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".connect.go") {
			return nil
		}
		fileSet := token.NewFileSet()
		file, parseErr := parser.ParseFile(fileSet, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, unquoteErr := strconv.Unquote(literal.Value)
			if unquoteErr != nil || !strings.HasPrefix(value, "/deephost.v1.") {
				return true
			}
			seen++
			if _, bad := httpRoutes[value]; bad {
				t.Errorf("client-only procedure %s is in httpRoutes", value)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if seen == 0 {
		t.Fatalf("no deephost procedures found under %s", root)
	}
}

func TestRawRoutesAreAllowlisted(t *testing.T) {
	for _, route := range rawRoutes {
		if _, ok := httpRoutes[route]; !ok {
			t.Errorf("raw route %s is not in httpRoutes", route)
		}
	}
	if _, ok := httpRoutes["other"]; !ok {
		t.Fatal(`httpRoutes must contain the "other" fallback`)
	}
}

func TestRPCTargetCollapsesUnknownProcedures(t *testing.T) {
	for _, procedure := range []string{
		"",
		"/",
		"/notaservice",
		"/deephost.v1.ControlPlaneService/ListApps",
		"/dns.v1.DNSService/NotAMethod",
		"/dns.v1.DNSService/ListZones/extra",
	} {
		service, method := rpcTarget(procedure)
		if procedure == "/dns.v1.DNSService/NotAMethod" {
			if service != "dns.v1.DNSService" || method != "unknown" {
				t.Errorf("rpcTarget(%q) = %q/%q, want dns.v1.DNSService/unknown", procedure, service, method)
			}
			continue
		}
		if service != "unknown" || method != "unknown" {
			t.Errorf("rpcTarget(%q) = %q/%q, want unknown/unknown", procedure, service, method)
		}
	}
}

func TestActivityKindsAreSortedAndBounded(t *testing.T) {
	kinds := ActivityKinds()
	if !slices.IsSorted(kinds) {
		t.Fatal("ActivityKinds must return a sorted list")
	}
	if !slices.Contains(kinds, "other") {
		t.Fatal(`ActivityKinds must contain the "other" fallback`)
	}
	for _, kind := range kinds {
		if _, ok := activityKinds[kind]; !ok {
			t.Errorf("ActivityKinds returned %q, which is not in the allowlist", kind)
		}
	}
}

// storeHelpers are the transaction helpers whose second argument is the
// db.operation label. Every call site passes a string literal except the one
// pass-through in zone.Store.updateZone, which forwards its caller's literal.
var storeHelpers = []string{"viewTx", "updateTx", "updateZone", "viewZone"}

// storeFiles are the two bbolt stores that report transactions.
var storeFiles = []string{"../zone/store.go", "../platform/store.go"}

// TestStoreOperationsCoverEveryReportedTransaction keeps the db.operation
// allowlist honest: a new store path that is not listed here collapses to
// "other", which is exactly the label that makes a metric useless.
func TestStoreOperationsCoverEveryReportedTransaction(t *testing.T) {
	seen := 0
	for _, path := range storeFiles {
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !slices.Contains(storeHelpers, selector.Sel.Name) {
				return true
			}
			literal, ok := call.Args[1].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				// zone.Store.updateZone forwards its caller's operation.
				return true
			}
			operation, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Errorf("%s: unquote %s: %v", path, literal.Value, err)
				return true
			}
			seen++
			if _, ok := storeOperations[operation]; !ok {
				t.Errorf("%s reports db.operation %q, which is not in storeOperations", path, operation)
			}
			return true
		})
	}
	if seen == 0 {
		t.Fatalf("no store transactions found in %v", storeFiles)
	}
	if _, ok := storeOperations["apply_record_set"]; !ok {
		t.Error(`storeOperations must contain "apply_record_set": every engine DNS write reports it`)
	}
}
