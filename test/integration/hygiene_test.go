package integration

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestPackageHygiene reads this package's own source and fails on the three
// shapes that have each, historically, cost this suite a wedged CI run.
//
// It is a meta-test rather than a lint rule because all three are local to this
// package: `http.Get` is perfectly fine in product code with a bounded client,
// `cmd.Wait` is fine wherever exactly one goroutine owns the process, and the
// watchdog exists nowhere else. golangci-lint has no vocabulary for "in THIS
// package, that call is a bug".
//
// It is AST-based rather than grep-based so it cannot be fooled by the same
// text inside a string literal or a comment -- including the ones in this file,
// which names every forbidden construct.
func TestPackageHygiene(t *testing.T) {
	startTest(t, defaultTestBudget)

	files, fset := parsePackageSources(t)

	var findings []string
	var exemptions []string

	for _, f := range files {
		ex := exemptionsIn(f.file, fset)
		report := func(rule string, pos token.Pos, msg string) {
			line := fset.Position(pos).Line
			where := fmt.Sprintf("%s:%d", f.name, line)
			if reason, ok := ex.covers(rule, line); ok {
				exemptions = append(exemptions, fmt.Sprintf("%s [%s] %s", where, rule, reason))
				return
			}
			findings = append(findings, fmt.Sprintf("%s: %s", where, msg))
		}

		checkUnboundedHTTP(f, report)
		checkForeignCmdWait(f, report)
		checkWatchdogInstalled(f, report)
	}

	// Exemptions are allowed but never silent: every one is printed on every
	// run, so a list that starts growing is visible rather than discovered later
	// by whoever is debugging the hang it caused.
	sort.Strings(exemptions)
	if len(exemptions) > 0 {
		t.Logf("hygiene exemptions in force (%d):\n  %s", len(exemptions), strings.Join(exemptions, "\n  "))
	}

	sort.Strings(findings)
	for _, f := range findings {
		t.Errorf("%s", f)
	}
	if len(findings) > 0 {
		t.Logf("each finding can be silenced with a `%s<rule> <reason>` comment on the offending line "+
			"or the line above it -- but read the rule's doc comment first; every one of them exists "+
			"because the construct wedged a real run", exemptionPrefix)
	}
}

// --- rule 1: HTTP that cannot be bounded -------------------------------------

// checkUnboundedHTTP rejects http.DefaultClient and the http package's
// top-level request helpers.
//
// All of them use http.DefaultClient, whose Timeout is zero: a server that
// accepts the connection and then never answers blocks the call FOREVER. The
// caller's poll loop then sails past its own deadline, the per-test watchdog
// eventually fires, and what should have been one failed assertion becomes a
// stack dump. Use apiClient (bounded per request), sseHTTPClient (bounded
// connect + per-frame), or a client of your own with an explicit budget.
func checkUnboundedHTTP(f parsedFile, report func(rule string, pos token.Pos, msg string)) {
	const rule = "unbounded-http"
	banned := map[string]bool{"Get": true, "Post": true, "PostForm": true, "Head": true}

	ast.Inspect(f.file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "http" {
			switch {
			case sel.Sel.Name == "DefaultClient":
				report(rule, sel.Pos(), "http.DefaultClient has no Timeout; use apiClient, sseHTTPClient, or a client with an explicit budget")
			case banned[sel.Sel.Name]:
				report(rule, sel.Pos(), fmt.Sprintf("http.%s goes through http.DefaultClient, which has no Timeout; use apiClient (or sseHTTPClient for a stream)", sel.Sel.Name))
			}
		}
		return true
	})
}

// --- rule 2: a second owner for a process ------------------------------------

// cmdWaitOwner is the ONE file allowed to call Cmd.Wait on a launched prox
// process: fixture_test.go, where proxRun starts exactly one waiter goroutine
// per launch and every other caller merely observes the channel it closes.
//
// exec.Cmd.Wait is documented as not safe for concurrent use, and both callers
// write Cmd.ProcessState. The defect this rule prevents is real and was in this
// suite: a `defer killProx(cmd)` plus a waitCmdExit that spawned its own
// `go cmd.Wait()`, so every timeout -- i.e. every test that was ALREADY failing
// -- buried its real assertion under a race report. See
// TestProxRun_KillDuringWaitExitIsRaceFree.
const cmdWaitOwner = "fixture_test.go"

func checkForeignCmdWait(f parsedFile, report func(rule string, pos token.Pos, msg string)) {
	const rule = "cmd-wait"
	if f.name == cmdWaitOwner {
		return
	}

	ast.Inspect(f.file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Wait" || len(call.Args) != 0 {
			return true
		}
		// Untyped AST, so the receiver is matched by name. Every exec.Cmd in
		// this package is spelled with "cmd" in its identifier; sync.WaitGroups
		// (wg.Wait) and channels are not.
		recv := exprText(sel.X)
		if !strings.Contains(strings.ToLower(recv), "cmd") {
			return true
		}
		report(rule, call.Pos(), fmt.Sprintf(
			"%s.Wait() outside %s: exec.Cmd.Wait is not safe for concurrent use, and one launch must have exactly one waiter (proxRun owns it)",
			recv, cmdWaitOwner))
		return true
	})
}

// --- rule 3: every test is bounded -------------------------------------------

// checkWatchdogInstalled requires every top-level test to call startTest.
//
// Without it a deadlocked test is bounded only by `go test -timeout`, twenty
// minutes away, and the failure it eventually produces names the package rather
// than the test -- which is exactly the diagnostic that has been missing.
// Subtests are deliberately NOT required to install their own: they inherit the
// parent's deadline through testDeadline's name walk.
func checkWatchdogInstalled(f parsedFile, report func(rule string, pos token.Pos, msg string)) {
	const rule = "no-watchdog"

	for _, decl := range f.file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Body == nil || !isTestFunc(fn) {
			continue
		}
		if callsStartTest(fn.Body) {
			continue
		}
		report(rule, fn.Pos(), fmt.Sprintf(
			"%s does not install a watchdog: add `startTest(t, defaultTestBudget)` as its first statement", fn.Name.Name))
	}
}

// isTestFunc reports whether fn is `func TestXxx(t *testing.T)`. TestMain, which
// takes *testing.M, is not one and has nothing to bound.
func isTestFunc(fn *ast.FuncDecl) bool {
	if !strings.HasPrefix(fn.Name.Name, "Test") || fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		return false
	}
	return exprText(fn.Type.Params.List[0].Type) == "*testing.T"
}

func callsStartTest(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "startTest" {
			found = true
		}
		return !found
	})
	return found
}

// --- exemptions ---------------------------------------------------------------

// exemptionPrefix marks a deliberate, reasoned violation:
//
//	resp, err := http.Get(u) //prox:allow-unbounded-http probing a server we
//	                         // control that always answers
//
// The marker must name the rule and carry a reason, and it applies to its own
// line and the line after it (so it can sit above a long statement). Every
// exemption in force is printed by TestPackageHygiene on every run.
const exemptionPrefix = "//prox:allow-"

type fileExemptions struct {
	// byRule maps rule -> line -> reason.
	byRule map[string]map[int]string
	// malformed are markers naming a rule but giving no reason.
	malformed []string
}

func (e fileExemptions) covers(rule string, line int) (string, bool) {
	lines, ok := e.byRule[rule]
	if !ok {
		return "", false
	}
	if reason, ok := lines[line]; ok {
		return reason, true
	}
	return "", false
}

// exemptionsIn collects every exemption marker in a file, expanding each to
// cover its own line and the next one.
func exemptionsIn(file *ast.File, fset *token.FileSet) fileExemptions {
	e := fileExemptions{byRule: map[string]map[int]string{}}
	for _, group := range file.Comments {
		for _, c := range group.List {
			text := strings.TrimSpace(c.Text)
			if !strings.HasPrefix(text, exemptionPrefix) {
				continue
			}
			body := strings.TrimPrefix(text, exemptionPrefix)
			rule, reason, _ := strings.Cut(body, " ")
			reason = strings.TrimSpace(reason)
			line := fset.Position(c.Pos()).Line
			if reason == "" {
				e.malformed = append(e.malformed, fmt.Sprintf("line %d: %s gives no reason", line, text))
				continue
			}
			if e.byRule[rule] == nil {
				e.byRule[rule] = map[int]string{}
			}
			e.byRule[rule][line] = reason
			e.byRule[rule][line+1] = reason
		}
	}
	return e
}

// --- source loading ------------------------------------------------------------

type parsedFile struct {
	name string
	file *ast.File
}

// parsePackageSources parses every *_test.go in this package's directory.
//
// It reads the directory rather than a hardcoded list so a new file is covered
// the moment it is added -- a rule that only checks the files someone
// remembered to enumerate is a rule that stops holding.
func parsePackageSources(t *testing.T) ([]parsedFile, *token.FileSet) {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	fset := token.NewFileSet()
	var out []parsedFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out = append(out, parsedFile{name: name, file: f})
	}
	if len(out) == 0 {
		t.Fatal("no *_test.go sources found; the hygiene rules would pass vacuously")
	}
	return out, fset
}

// exprText renders an expression the way it was written, for use in messages
// and in the receiver-name heuristic above.
func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprText(v.X) + "." + v.Sel.Name
	case *ast.StarExpr:
		return "*" + exprText(v.X)
	case *ast.IndexExpr:
		return exprText(v.X) + "[" + exprText(v.Index) + "]"
	case *ast.CallExpr:
		return exprText(v.Fun) + "(...)"
	case *ast.ParenExpr:
		return "(" + exprText(v.X) + ")"
	default:
		return fmt.Sprintf("%T", e)
	}
}

// TestPackageHygiene_MalformedExemptions fails on an exemption marker that
// names a rule but gives no reason.
//
// An exemption with no reason is the failure mode that makes an exemption
// mechanism worthless: it silences the rule while recording nothing about why,
// so the next reader cannot tell a considered decision from a dodge.
func TestPackageHygiene_MalformedExemptions(t *testing.T) {
	startTest(t, defaultTestBudget)

	files, fset := parsePackageSources(t)
	for _, f := range files {
		for _, m := range exemptionsIn(f.file, fset).malformed {
			t.Errorf("%s: %s", f.name, m)
		}
	}
}
