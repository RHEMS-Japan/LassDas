package attendant

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// The reception's question goes out from one place, and the stop has to be
// read on the way. What went wrong live was placement, not logic: the read
// existed a few lines further down, on the path a ready ticket takes, and
// the question path had none — so a requester who wrote 「停止」 while the
// gate ran was asked anyway (2026-09-25). A behavioural test of that path
// would need the ledger, the card board and the worker binaries; this pins
// the one thing that was wrong, which is that the call is there and comes
// first.
func TestTheQuestionPathReadsTheStopBeforeItAsks(t *testing.T) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "chains.go", nil, 0)
	if err != nil {
		t.Fatalf("chains.go does not parse: %v", err)
	}
	branch := questionBranch(t, file)
	stopAt, askAt := token.NoPos, token.NoPos
	ast.Inspect(branch, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		switch function := call.Fun.(type) {
		case *ast.Ident:
			if function.Name == "stoppedWhileWorking" && !stopAt.IsValid() {
				stopAt = call.Pos()
			}
		case *ast.SelectorExpr:
			if function.Sel.Name == "AskQuestion" && !askAt.IsValid() {
				askAt = call.Pos()
			}
		}
		return true
	})
	if !askAt.IsValid() {
		t.Fatal("the question branch no longer asks the question; this test is looking at the wrong code")
	}
	if !stopAt.IsValid() {
		t.Fatal("the question is posted without reading the stop first")
	}
	if stopAt > askAt {
		t.Fatalf("the stop is read at %s, after the question is asked at %s",
			fileSet.Position(stopAt), fileSet.Position(askAt))
	}
}

// questionBranch is the block that runs when the reception produced a
// question rather than a verdict.
func questionBranch(t *testing.T, file *ast.File) ast.Node {
	t.Helper()
	var branch ast.Node
	ast.Inspect(file, func(node ast.Node) bool {
		statement, isIf := node.(*ast.IfStmt)
		if !isIf || branch != nil {
			return true
		}
		comparison, isComparison := statement.Cond.(*ast.BinaryExpr)
		if !isComparison {
			return true
		}
		selector, isSelector := comparison.X.(*ast.SelectorExpr)
		if isSelector && selector.Sel.Name == "QuestionDecisionPath" {
			branch = statement.Body
		}
		return true
	})
	if branch == nil {
		t.Fatal("no branch acts on a question the reception produced; this test is looking at the wrong code")
	}
	return branch
}

// The helper's own name is part of what the test above reads, so a rename
// that forgets the test would pass it while proving nothing.
func TestTheStopHelperIsNamedTheWayTheAskPathCallsIt(t *testing.T) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "chains.go", nil, 0)
	if err != nil {
		t.Fatalf("chains.go does not parse: %v", err)
	}
	for _, declaration := range file.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if isFunction && function.Name.Name == "stoppedWhileWorking" {
			return
		}
	}
	t.Fatal("stoppedWhileWorking is gone; TestTheQuestionPathReadsTheStopBeforeItAsks is looking for a call that cannot exist")
}

// A guard against the test above passing on a file it cannot see.
func TestTheParsedChainsFileIsTheRealOne(t *testing.T) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "chains.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("chains.go does not parse: %v", err)
	}
	if file.Name.Name != "attendant" || !strings.Contains(fileSet.Position(file.Pos()).Filename, "chains.go") {
		t.Fatalf("parsed %s from package %s", fileSet.Position(file.Pos()).Filename, file.Name.Name)
	}
}
