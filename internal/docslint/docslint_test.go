// Package docslint holds regression guards for the docs/examples shipped
// with the repo (README.md, docs/, skills/). It has no runtime code of its
// own -- it exists purely to keep documentation and the codebase from
// drifting apart in ways a normal `go build` can't catch.
package docslint

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestNoActivePinnedAPIPortInExamples is a regression guard for plan 020
// (D0/C0): a shipped example must not teach copy-paste users to pin
// api.port. api.port is dynamic by default (0 = auto-assigned free port),
// and that is the SAFE default -- pinning a fixed port (classically 5555)
// is what causes cross-project collisions once more than one prox-managed
// project exists on a machine. See internal/constants/constants.go
// (DefaultAPIHost) and internal/config for the runtime behavior this test
// is protecting.
//
// The test scans every ```yaml fenced code block under README.md, docs/,
// and skills/ for an ACTIVE (i.e. non-comment) `api.port` assignment to a
// nonzero value, in either of the two forms used in this repo's docs:
//
//	api:
//	  port: 5555
//
//	api: { port: 5555 }
//
// A commented-out line (e.g. `# api: { port: 5555 }`) is never flagged --
// it's inert YAML, not something a copy-paste reader inherits.
//
// # Marker: legitimately documenting how to pin a port
//
// Some docs (e.g. the "HTTP API" section of docs/getting-started/quick-start.md)
// deliberately show HOW to pin api.port, as an opt-in feature. That's a
// legitimate, different thing from presenting a pinned port as the baseline
// shape of a prox.yaml. To add such a demonstration without tripping this
// guard, put a marker comment on its own line directly above the opening
// ``` fence:
//
//	<!-- doclint:pin-example -->
//	```yaml
//	api:
//	  port: 5555
//	```
//
// The marker just needs to appear within a few lines above the fence and
// contain the literal text "doclint:pin-example" -- see quick-start.md for
// a real example.
//
// # Deliberate limits
//
// This is a guard against the specific regression plan 020 fixed, not a
// general YAML or CommonMark parser. Known and accepted gaps (raised by the
// codex review of C0 and consciously not closed -- closing them would mean
// vendoring a markdown parser for a doc-hygiene check):
//
//   - Opening fences must start at column 0, so a yaml example nested inside
//     a list item or blockquote is not scanned. Tilde fences and fences with
//     more than three backticks are likewise not recognized.
//   - Only unquoted keys are matched: `"api": {"port": 5555}` slips through.
//     No doc in this repo uses quoted YAML keys.
//
// Both gaps are false NEGATIVES (something slips past), never false
// positives, so the guard can annoy nobody into deleting it. If a doc ever
// legitimately needs one of those forms, extend the detector rather than
// removing the check.
func TestNoActivePinnedAPIPortInExamples(t *testing.T) {
	root := repoRoot(t)

	var files []string
	files = append(files, filepath.Join(root, "README.md"))
	files = append(files, mdFilesUnder(t, filepath.Join(root, "docs"))...)
	files = append(files, mdFilesUnder(t, filepath.Join(root, "skills"))...)

	for _, f := range files {
		checkFileForPinnedAPIPort(t, root, f)
	}

	// The repo's own prox.yaml is the fourth baseline example C0 unpinned, and
	// it is the one people read to see "how it's done" in the real thing. It is
	// raw YAML rather than a fenced block, so feed it to the detector directly
	// (codex review finding: it was omitted, leaving a quarter of what this
	// guard protects unguarded).
	checkBareYAMLForPinnedAPIPort(t, root, filepath.Join(root, "prox.yaml"))
}

// checkBareYAMLForPinnedAPIPort runs the detector over a whole .yaml file
// (no markdown fences involved).
func checkBareYAMLForPinnedAPIPort(t *testing.T, root, path string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	if port, portLine, ok := findActivePinnedPort(strings.Split(string(data), "\n")); ok {
		t.Errorf("%s:%d: pins api.port to %d; the repo's own config must model the"+
			" dynamic default (plan 020 D0)", rel, portLine+1, port)
	}
}

// repoRoot resolves the repository root from this test's package directory
// (internal/docslint), following the same relative-path convention used by
// internal/config's tests (e.g. filepath.Join("..", "..", "testdata", ...)).
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected repo root at %s (go.mod not found): %v", root, err)
	}
	return root
}

// mdFilesUnder returns every *.md file under dir, recursively.
func mdFilesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".md") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return out
}

const pinMarker = "doclint:pin-example"

var (
	fenceOpenRe  = regexp.MustCompile("^```ya?ml\\s*$")
	fenceCloseRe = regexp.MustCompile("^```\\s*$")
	// flatAPIPortRe matches the single-line `api: { ... port: N ... }` form.
	flatAPIPortRe = regexp.MustCompile(`^api:\s*\{[^}]*\bport:\s*([0-9]+)`)
	// nestedAPIRe matches an exact top-of-block `api:` mapping key.
	nestedAPIRe = regexp.MustCompile(`^api:\s*(#.*)?$`)
	// nestedPortRe matches a `port: N` line nested under an `api:` mapping.
	nestedPortRe = regexp.MustCompile(`^port:\s*([0-9]+)\b`)
)

// checkFileForPinnedAPIPort scans a single markdown file for yaml fenced
// blocks that actively pin api.port to a nonzero value without a preceding
// doclint:pin-example marker.
func checkFileForPinnedAPIPort(t *testing.T, root, path string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}

	inFence := false
	fenceStart := -1
	// prevFenceEnd is the index of the last fence CLOSE seen, so a marker
	// belonging to an earlier block cannot leak forward and exempt this one
	// (codex review finding).
	prevFenceEnd := -1
	var fenceBody []string

	report := func(body []string, start int) {
		port, portLine, ok := findActivePinnedPort(body)
		if !ok || hasMarkerAbove(lines, start, prevFenceEnd) {
			return
		}
		// start is the 0-based index of the ``` line; the body begins on the
		// NEXT line, hence +2 to reach a 1-based line number (codex review
		// finding: this was reporting one line too low).
		t.Errorf(
			"%s:%d: yaml example pins api.port to %d without a"+
				" %q marker above the fence (see docslint_test.go"+
				" doc comment for how to add a legitimate pinning"+
				" demonstration)",
			rel, start+2+portLine, port, pinMarker,
		)
	}

	for i, line := range lines {
		if !inFence {
			if fenceOpenRe.MatchString(line) {
				inFence = true
				fenceStart = i
				fenceBody = nil
			}
			continue
		}

		if fenceCloseRe.MatchString(line) {
			report(fenceBody, fenceStart)
			inFence = false
			prevFenceEnd = i
			continue
		}

		fenceBody = append(fenceBody, line)
	}

	// An unterminated fence runs to EOF per CommonMark; analyze it rather than
	// silently dropping it (codex review finding).
	if inFence {
		report(fenceBody, fenceStart)
	}
}

// findActivePinnedPort scans the lines of a single yaml fenced block for an
// active (non-comment) api.port assignment to a nonzero value. It returns
// the port, the 0-based line offset within body where the port was found,
// and whether a match was found.
func findActivePinnedPort(body []string) (port int, lineOffset int, found bool) {
	inAPIBlock := false

	for i, raw := range body {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " "))

		if indent > 0 {
			// A nested line only matters while we're inside a top-level
			// api: mapping being tracked.
			if inAPIBlock {
				if m := nestedPortRe.FindStringSubmatch(trimmed); m != nil {
					if p, err := strconv.Atoi(m[1]); err == nil && p != 0 {
						return p, i, true
					}
				}
			}
			continue
		}

		// indent == 0: any top-level key ends a previously-tracked api:
		// mapping (dedent, or a same-level sibling key).
		inAPIBlock = false

		if m := flatAPIPortRe.FindStringSubmatch(trimmed); m != nil {
			if p, err := strconv.Atoi(m[1]); err == nil && p != 0 {
				return p, i, true
			}
			continue
		}

		// Only a top-level (column 0) `api:` key is the API-server config
		// block; a nested `api:` (e.g. a `services: { api: { port: ... } }`
		// proxy service happening to be named "api") is a different thing
		// and must not be treated as api.port.
		if nestedAPIRe.MatchString(trimmed) {
			inAPIBlock = true
		}
	}

	return 0, 0, false
}

// hasMarkerAbove reports whether one of the (up to 5) lines immediately
// preceding fenceStart contains the pinMarker text. The search never reaches
// back past prevFenceEnd (the previous fence's closing line), so a marker
// written for one block cannot exempt a later adjacent one.
func hasMarkerAbove(lines []string, fenceStart, prevFenceEnd int) bool {
	const lookback = 5
	start := max(fenceStart-lookback, prevFenceEnd+1)
	for i := start; i < fenceStart; i++ {
		if strings.Contains(lines[i], pinMarker) {
			return true
		}
	}
	return false
}

// TestFindActivePinnedPort unit-tests the detector directly, independent of
// which files currently exist under README.md/docs/skills. It pins down the
// exact regression this guard exists to catch (the pre-plan-020 README
// "Expanded Form" block pinned api.port: 5555 as the baseline shape) plus
// the shapes that must NOT be flagged: a commented-out example, an unrelated
// nested "api:" key (e.g. a services.api proxy service, which shares the
// key name but is not the API-server config), and an explicit `port: 0`
// (dynamic, the safe default).
func TestFindActivePinnedPort(t *testing.T) {
	tests := []struct {
		name     string
		body     []string
		wantPort int
		wantOK   bool
	}{
		{
			name: "pre-020 README expanded form is flagged",
			body: []string{
				"api:",
				"  port: 5555",
				"  host: 127.0.0.1",
				"",
				"env_file: .env",
			},
			wantPort: 5555,
			wantOK:   true,
		},
		{
			name: "flat inline form is flagged",
			body: []string{
				"api: { port: 5555 }",
				"",
				"processes:",
				"  web: npm run dev",
			},
			wantPort: 5555,
			wantOK:   true,
		},
		{
			name: "commented-out flat form is not flagged",
			body: []string{
				"# api: { port: 5555 }   # optional: pin the API port; omit for a dynamic one",
				"",
				"processes:",
				"  web: npm run dev",
			},
			wantOK: false,
		},
		{
			name: "explicit dynamic port (0) is not flagged",
			body: []string{
				"api:",
				"  port: 0",
			},
			wantOK: false,
		},
		{
			name: "nested services.api proxy service is not flagged",
			body: []string{
				"services:",
				"  app: 3000",
				"  api:",
				"    port: 8000",
				"    host: localhost",
			},
			wantOK: false,
		},
		{
			name: "no api key at all",
			body: []string{
				"processes:",
				"  web: npm run dev",
			},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port, _, ok := findActivePinnedPort(tt.body)
			if ok != tt.wantOK {
				t.Fatalf("findActivePinnedPort() ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && port != tt.wantPort {
				t.Fatalf("findActivePinnedPort() port = %d, want %d", port, tt.wantPort)
			}
		})
	}
}
