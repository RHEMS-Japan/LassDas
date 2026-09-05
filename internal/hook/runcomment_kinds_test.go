package hook

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// Every RunCommentKind constant in this package is either posted through the
// run store (StoreRunCommentKinds) or found by scanning the ticket for its
// marker (the kinds named here). A new kind that is neither is the drift
// that left the investigation report unposted (live 2026-09-05): the hook
// knew the kind, the store did not.
func TestEveryRunCommentKindIsStoredOrMarkerScanned(t *testing.T) {
	markerScanned := map[RunCommentKind]bool{
		RunCommentResolved: true, RunCommentBudgetHold: true, RunCommentSessionHold: true,
		RunCommentStreakHold: true, RunCommentStreakResolved: true,
	}
	stored := map[RunCommentKind]bool{}
	for _, kind := range StoreRunCommentKinds() {
		stored[kind] = true
	}
	file, err := parser.ParseFile(token.NewFileSet(), "runcomment_protocol.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := 0
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := value.Type.(*ast.Ident)
			if !ok || ident.Name != "RunCommentKind" || len(value.Values) != 1 {
				continue
			}
			lit, ok := value.Values[0].(*ast.BasicLit)
			if !ok {
				continue
			}
			raw, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			declared++
			kind := RunCommentKind(raw)
			if stored[kind] == markerScanned[kind] {
				t.Errorf("run comment kind %q (%s) must be exactly one of: posted through the store, scanned by marker", kind, value.Names[0].Name)
			}
		}
	}
	if declared < 8 {
		t.Fatalf("parsed only %d RunCommentKind constants; the declaration shape changed", declared)
	}
}
