package importer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestOnlyTheGuardChangesFiles keeps every change to files and folders in
// the guard, which checks that Garner owns what it changes (GRR-31).
func TestOnlyTheGuardChangesFiles(t *testing.T) {
	changes := map[string]bool{
		"Chmod": true, "Chown": true, "Chtimes": true, "Create": true, "Link": true, "Mkdir": true, "MkdirAll": true,
		"OpenFile": true, "Remove": true, "RemoveAll": true, "Rename": true, "Symlink": true, "Truncate": true, "WriteFile": true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "os" && changes[sel.Sel.Name] {
					t.Errorf("%s: os.%s changes files outside the guard", fset.Position(sel.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
}
