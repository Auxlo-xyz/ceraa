package telegram

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Cancelling a run is by command only. A companion "tap Stop" message was tried
// and removed as clutter, so nothing in the package may post a Stop button again.
func TestNoStopButtonIsEverPosted(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	fset := token.NewFileSet()
	var offenders []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			// Both a declaration and a call are an offence. Checking calls alone
			// misses the worse case: a helper defined to post a Stop button and then
			// never called is dead code that looks like the feature is still there.
			switch typed := node.(type) {
			case *ast.FuncDecl:
				if isStopButtonHelper(typed.Name.Name) {
					position := fset.Position(typed.Pos())
					offenders = append(offenders, position.Filename+":"+strconv.Itoa(position.Line)+" declares "+typed.Name.Name+"()")
				}
			case *ast.CallExpr:
				ident, ok := typed.Fun.(*ast.Ident)
				if !ok || !isStopButtonHelper(ident.Name) {
					return true
				}
				position := fset.Position(typed.Pos())
				offenders = append(offenders, position.Filename+":"+strconv.Itoa(position.Line)+" calls "+ident.Name+"()")
			}
			return true
		})
	}
	if len(offenders) > 0 {
		t.Errorf("a Stop button is being posted again: %v", offenders)
	}
}

// isStopButtonHelper recognises a helper whose job is posting a Stop control.
func isStopButtonHelper(name string) bool {
	return strings.HasPrefix(name, "sendStop") || strings.HasPrefix(name, "postStop")
}

// The run:stop callback is retained only to answer buttons that predate the
// removal, and it must not cancel anything.
func TestLegacyRunStopCallbackDoesNotCancel(t *testing.T) {
	source, err := os.ReadFile("callback.go")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(source)
	start := strings.Index(text, `strings.HasPrefix(data, "run:stop")`)
	if start < 0 {
		t.Fatal("the legacy run:stop handler is gone; old buttons would be silently ignored")
	}
	end := strings.Index(text[start:], "\n\t}\n")
	if end < 0 {
		t.Fatal("could not delimit the run:stop handler")
	}
	handler := text[start : start+end]
	if strings.Contains(handler, "stopActiveRun") {
		t.Error("the legacy run:stop handler still cancels a run; it should only point at /stop")
	}
	if !strings.Contains(handler, "/stop") {
		t.Error("the legacy handler does not tell the user about /stop")
	}
}
