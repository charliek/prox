package proxyd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNoTestConstructsTheCertManagerWithoutPinningTrust stops a unit test in
// this package from shelling out to the developer's real mkcert.
//
// EnsureDomain applies the CA-trust verdict on EVERY call (plan 028 B1), so a
// test holding a real *MultiDomainCertManager reaches certs.SharedTrust(),
// which will run `mkcert -CAROOT` and issue a throwaway certificate on any
// machine that has mkcert installed. Overriding `generate` — which used to be
// enough, because the verdict was only applied during generation — no longer
// keeps mkcert out.
//
// The failure mode is nasty precisely because it is not a failure: the tests
// still pass, just slower, with behaviour that depends on whether the machine
// running them happens to have mkcert and a CA. So the rule is enforced
// structurally rather than trusted to reviewers: every construction of the real
// manager inside a _test.go file in this package must also assign
// resolveTrust.
func TestNoTestConstructsTheCertManagerWithoutPinningTrust(t *testing.T) {
	const ctor = "NewMultiDomainCertManager"

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	require.NoError(t, err, "parsing this package's test files")

	type site struct {
		file string
		line int
		fn   string
	}
	var unpinned []site
	seen := 0

	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				// Does this function construct the real manager, and if so does
				// it also pin resolveTrust? Both questions are answered over the
				// same function body, since that is the scope a test sets up in.
				constructs, pins := false, false
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch v := n.(type) {
					case *ast.CallExpr:
						if id, ok := v.Fun.(*ast.Ident); ok && id.Name == ctor {
							constructs = true
						}
					case *ast.SelectorExpr:
						if v.Sel != nil && v.Sel.Name == "resolveTrust" {
							pins = true
						}
					}
					return true
				})
				if !constructs {
					continue
				}
				seen++
				if !pins {
					unpinned = append(unpinned, site{
						file: name,
						line: fset.Position(fn.Pos()).Line,
						fn:   fn.Name.Name,
					})
				}
			}
		}
	}

	// A walk that matched nothing would be a test that passes forever.
	require.NotZero(t, seen,
		"found no %s call in any test file — the AST walk is broken, not the tests", ctor)

	for _, s := range unpinned {
		t.Errorf("%s:%d: %s constructs a real %s without assigning resolveTrust, "+
			"so EnsureDomain will shell out to the machine's actual mkcert. "+
			"Add: m.resolveTrust = func() certs.TrustVerdict { return certs.TrustVerdict{} }",
			s.file, s.line, s.fn, ctor)
	}
}
