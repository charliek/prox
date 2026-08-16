package domain

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAllProcessStates_MatchesTheEnumDeclaration is the meta-test that makes
// AllProcessStates trustworthy.
//
// Every exhaustive predicate table in this repo -- IsLive/IsTerminalFailure
// here, isTerminalFailureState in internal/cli, stateLabel in internal/tui --
// is exhaustive only because it iterates AllProcessStates(). That reduces the
// whole question to "does AllProcessStates() actually list every state?", and
// nothing in Go answers it: constants are not enumerable at runtime, so a 9th
// ProcessState added to the const block but forgotten here would leave every
// one of those tables silently passing over 8 of 9 states.
//
// So this test asks the SOURCE instead. It parses the package's own
// declarations, collects every constant explicitly typed ProcessState, and
// requires the set to be exactly what AllProcessStates() returns. Adding a
// state to the const block without adding it here now fails the build, which
// is the only version of this guarantee that survives someone who has never
// read this comment. (Same technique as test/integration/hygiene_test.go,
// plan 027.)
func TestAllProcessStates_MatchesTheEnumDeclaration(t *testing.T) {
	declared := processStateConstsFromSource(t)

	returned := make(map[string]bool, len(AllProcessStates()))
	for _, s := range AllProcessStates() {
		require.False(t, returned[string(s)],
			"AllProcessStates() returns %q more than once", s)
		returned[string(s)] = true
	}

	for value, name := range declared {
		assert.True(t, returned[value],
			"const %s (%q) is declared as a ProcessState but AllProcessStates() omits it -- "+
				"add it there, or every exhaustive predicate table in this repo silently skips it",
			name, value)
	}
	for value := range returned {
		_, ok := declared[value]
		assert.True(t, ok,
			"AllProcessStates() returns %q, which is not declared as a ProcessState constant "+
				"in this package -- a stale or misspelled entry", value)
	}
}

// processStateConstsFromSource returns value -> constant name for every
// constant in this package explicitly typed ProcessState. Test files are
// excluded: a fixture declaring its own state must not count as part of the
// enum.
func processStateConstsFromSource(t *testing.T) map[string]string {
	t.Helper()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	require.NoError(t, err, "parsing the domain package's own source")

	found := make(map[string]string)
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					ident, ok := vs.Type.(*ast.Ident)
					if !ok || ident.Name != "ProcessState" {
						continue
					}
					for i, name := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						lit, ok := vs.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						value, err := strconv.Unquote(lit.Value)
						require.NoError(t, err, "unquoting %s's value", name.Name)
						found[value] = name.Name
					}
				}
			}
		}
	}

	// A parser that silently matched nothing would make this whole test a
	// no-op that passes forever.
	require.NotEmpty(t, found,
		"found no ProcessState constants in the package source -- the AST walk is broken, "+
			"not the enum")
	return found
}
