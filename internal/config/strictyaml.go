package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Allowed key sets for the typed top-level blocks (plan 016 W2). These mirror
// the struct tags on Config/rawProxyConfig/rawCaptureConfig/APIConfig/
// CertsConfig, which decode leniently (unknown keys are silently dropped), so
// each set must stay in step with its struct. The per-entry sets for
// processes:/services:/dependencies:/tasks: live with their parsers
// (config.go, dependency.go) -- those blocks are keyed by user-chosen NAMES
// and are never key-checked by this pass.
var (
	topLevelAllowedKeys = map[string]struct{}{
		"api": {}, "env_file": {}, "processes": {}, "proxy": {}, "services": {},
		"certs": {}, "dependencies": {}, "tasks": {}, "shutdown_timeout": {},
	}
	proxyAllowedKeys = map[string]struct{}{
		"enabled": {}, "http_port": {}, "https_port": {}, "domain": {}, "capture": {},
	}
	captureAllowedKeys = map[string]struct{}{
		"enabled": {}, "max_body_size": {}, "disk_budget": {},
	}
	apiAllowedKeys   = map[string]struct{}{"port": {}, "host": {}, "auth": {}}
	certsAllowedKeys = map[string]struct{}{"dir": {}, "auto_generate": {}}

	// schemaAllowedKeys is the complete list of document paths that carry a
	// FIXED schema, keyed by dotted path ("" is the top level). The walk
	// descends through the whole document to find aliased duplicate keys, but
	// key-checks only at these five paths: everything else is either
	// user-named (processes.<name>, services.<name>, env vars) or owned by
	// another strict parser (dependencies/tasks, and the process/service
	// entries themselves).
	schemaAllowedKeys = map[string]map[string]struct{}{
		"":              topLevelAllowedKeys,
		"proxy":         proxyAllowedKeys,
		"proxy.capture": captureAllowedKeys,
		"api":           apiAllowedKeys,
		"certs":         certsAllowedKeys,
	}
)

// maxAliasHops bounds alias-chain dereferencing. yaml.v3 resolves an alias to
// its anchor node in one hop, so this is pure belt-and-braces against a
// malformed node graph -- the real cycle protection is structuralWalker.active.
const maxAliasHops = 100

// checkDocumentStructure walks the yaml.Node tree of an already-decoded config
// and reports the structural defects the generic map form cannot show (plan 016
// W2). It runs only AFTER the raw decode in Parse has succeeded (the W0
// ordering contract), so everything it can reach is YAML that yaml.v3 itself
// accepted. Two jobs:
//
//  1. Aliased duplicate keys, ANYWHERE in the document: a key node that is an
//     alias resolves to its anchor's scalar value, and a generic map silently
//     collapses it onto an existing sibling (last write wins). Reported as
//     `<path>: duplicate key %q`. Literal duplicates never reach here -- the
//     raw decode already rejected them with a line number.
//  2. Unknown keys in the five fixed-schema blocks (see schemaAllowedKeys),
//     reported as `<path>: unknown field %q` in dependency.go's exact format.
//     Keys are checked wherever they take effect, so a `<<` merge is expanded
//     (see walkMapping) and cannot be used to smuggle a typo -- or a whole
//     typo'd sub-block -- past its destination's schema.
//
// Errors come back as strings for Parse to batch and sort with the rest of the
// structural errors. A yaml.Node decode failure returns no errors rather than a
// second opinion on YAML syntax: the raw decode owns that error (W0).
func checkDocumentStructure(data []byte) []string {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	root := &doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return nil
		}
		root = root.Content[0]
	}
	// An empty document (or a comment-only one) decodes to the zero Node.
	if root.Kind == 0 {
		return nil
	}
	w := &structuralWalker{
		active:     make(map[*yaml.Node]bool),
		merging:    make(map[*yaml.Node]bool),
		dupChecked: make(map[*yaml.Node]bool),
	}
	w.walkValue(root, "")
	return w.errs
}

// structuralWalker carries the state of one checkDocumentStructure pass.
//
// active is the set of container nodes currently on the walk stack, which is
// what makes a cyclic alias (`proxy: &x {capture: *x}` -- accepted by the raw
// decode when it lands in a typed block) terminate instead of hang. merging is
// the same guard for merge expansion.
//
// dupChecked records the mapping nodes whose own keys have already been scanned
// for duplicates, so one defect is reported once even when the mapping is
// reachable from several places: an anchored mapping is walked at its natural
// position AND wherever it is aliased or merged in. It gates duplicate
// reporting only -- schema checks are per-path and must run at every
// destination.
type structuralWalker struct {
	errs       []string
	active     map[*yaml.Node]bool
	merging    map[*yaml.Node]bool
	dupChecked map[*yaml.Node]bool
}

// keyIdent is the identity of a mapping key for duplicate detection: the
// resolved tag AND value. Tag-blind comparison would be wrong -- `"1"` (!!str)
// and an alias resolving to `1` (!!int) are DISTINCT keys per the YAML spec,
// and flagging them would break valid config.
type keyIdent struct {
	tag   string
	value string
}

func (w *structuralWalker) errf(format string, args ...interface{}) {
	w.errs = append(w.errs, fmt.Sprintf(format, args...))
}

// walkValue descends into one value node at the given path, dereferencing a
// whole-block alias (`proxy: *shared`) first so the target is checked against
// the path it was aliased INTO. Scalars are leaves. The active-set guard is the
// single cycle break for the whole walk: `active` holds only the current
// descent stack, so hitting a member is a true back-edge — a self-referential
// alias. That is never a meaningful config (and its contents could not be
// schema-checked), so it is an error rather than a silent skip.
func (w *structuralWalker) walkValue(node *yaml.Node, path string) {
	target, ok := derefAlias(node)
	if !ok || (target.Kind != yaml.MappingNode && target.Kind != yaml.SequenceNode) {
		return
	}
	if w.active[target] {
		w.errf("%s: circular alias", path)
		return
	}
	w.active[target] = true
	defer delete(w.active, target)

	if target.Kind == yaml.SequenceNode {
		// Nothing in the schema is a sequence of mappings, but the walk still
		// descends for job 1. Indexing the path keeps duplicate-key errors
		// unambiguous and can never collide with a schema path.
		for i, item := range target.Content {
			w.walkValue(item, fmt.Sprintf("%s[%d]", path, i))
		}
		return
	}
	w.walkMapping(target, path)
}

// walkMapping runs both jobs on one mapping node (Content alternates key,
// value) in two passes, because YAML merge precedence runs that way: a mapping's
// OWN keys always win over anything a `<<` brings in.
//
// Pass 1 takes the explicit keys -- duplicate detection, schema check, descend.
// Pass 2 expands each merge in order and treats the keys that actually TAKE
// EFFECT (not shadowed by an explicit key or an earlier merge source) exactly
// like explicit keys of this mapping: same schema check, same descent at the
// destination's child path. That is what stops a typo from hiding behind a
// merge, at any depth -- a merged-in `proxy:` block is validated as `proxy`.
func (w *structuralWalker) walkMapping(node *yaml.Node, path string) {
	allowed, schema := schemaAllowedKeys[path]
	claimed := make(map[keyIdent]struct{}, len(node.Content)/2)
	reportDupes := !w.dupChecked[node]
	w.dupChecked[node] = true

	var merges []*yaml.Node
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode, valueNode := node.Content[i], node.Content[i+1]

		// A real merge key: yaml.v3 tags it !!merge, whereas a quoted "<<" is
		// an ordinary string key and falls through to the checks below. The
		// token itself is never an unknown field. Deferred to pass 2.
		if isMergeKey(keyNode) {
			merges = append(merges, valueNode)
			continue
		}

		ident, ok := resolveKeyIdent(keyNode)
		if !ok {
			// A non-scalar key (e.g. an alias to a mapping) -- yaml.v3 rejects
			// those at the raw decode, so this is unreachable defensive code.
			continue
		}
		if _, dup := claimed[ident]; dup {
			// Only reachable when one of the two keys is an alias: literal
			// duplicates errored at the raw decode (W0). The schema check is
			// skipped here because the first occurrence already made it -- one
			// report per distinct defect.
			if reportDupes {
				w.errf("%s: duplicate key %q", pathLabel(path), ident.value)
			}
			w.walkValue(valueNode, childPath(path, ident.value))
			continue
		}
		claimed[ident] = struct{}{}
		w.checkKey(ident, path, allowed, schema)
		w.walkValue(valueNode, childPath(path, ident.value))
	}

	for _, mergeValue := range merges {
		w.expandMerge(mergeValue, path, claimed, allowed, schema)
	}
}

// expandMerge applies one merge value to the destination mapping at path. The
// value may be a mapping, an alias to one, or a sequence of either (a
// multi-source merge, where the FIRST source wins); nested merges inside a
// merged mapping expand too, after that mapping's own keys.
//
// claimed carries the destination's precedence state and is updated as sources
// are consumed, so overlap with an explicit key or an earlier source is
// silently skipped -- that overlap is the spec-defined defaults idiom, never a
// duplicate. Duplicates WITHIN one source mapping are a different matter and
// are reported (at the destination path, where the collapse is felt), since a
// merge source is otherwise never scanned for aliased duplicate keys.
func (w *structuralWalker) expandMerge(node *yaml.Node, path string, claimed map[keyIdent]struct{}, allowed map[string]struct{}, schema bool) {
	target, ok := derefAlias(node)
	if !ok || w.merging[target] {
		return
	}
	w.merging[target] = true
	defer delete(w.merging, target)

	if target.Kind == yaml.SequenceNode {
		for _, item := range target.Content {
			w.expandMerge(item, path, claimed, allowed, schema)
		}
		return
	}
	if target.Kind != yaml.MappingNode {
		return
	}

	reportDupes := !w.dupChecked[target]
	w.dupChecked[target] = true
	sourceSeen := make(map[keyIdent]struct{}, len(target.Content)/2)
	var nested []*yaml.Node
	for i := 0; i+1 < len(target.Content); i += 2 {
		keyNode, valueNode := target.Content[i], target.Content[i+1]
		if isMergeKey(keyNode) {
			nested = append(nested, valueNode)
			continue
		}
		ident, ok := resolveKeyIdent(keyNode)
		if !ok {
			continue
		}
		if _, dup := sourceSeen[ident]; dup {
			if reportDupes {
				w.errf("%s: duplicate key %q", pathLabel(path), ident.value)
			}
			continue
		}
		sourceSeen[ident] = struct{}{}
		if _, taken := claimed[ident]; taken {
			// Shadowed by an explicit key or an earlier merge source: it does
			// not take effect here, so it is neither checked nor descended into
			// at this path. (It is still checked wherever it DOES take effect.)
			continue
		}
		claimed[ident] = struct{}{}
		w.checkKey(ident, path, allowed, schema)
		w.walkValue(valueNode, childPath(path, ident.value))
	}
	for _, nestedValue := range nested {
		w.expandMerge(nestedValue, path, claimed, allowed, schema)
	}
}

// checkKey reports a key that is not in the destination's allowed set, for the
// five fixed-schema paths only.
func (w *structuralWalker) checkKey(ident keyIdent, path string, allowed map[string]struct{}, schema bool) {
	if !schema {
		return
	}
	if _, ok := allowed[ident.value]; !ok {
		w.errf("%s: unknown field %q", pathLabel(path), ident.value)
	}
}

// isMergeKey reports whether a key node is a YAML merge key by yaml.v3's own
// rule: the resolved !!merge tag on a plain `<<` scalar. A quoted "<<" resolves
// to !!str and is therefore an ordinary (and, in a typed block, unknown) field.
func isMergeKey(node *yaml.Node) bool {
	return node != nil && node.Kind == yaml.ScalarNode && node.Tag == "!!merge" && node.Value == "<<"
}

// resolveKeyIdent returns the identity of a mapping key, dereferencing an alias
// key node to its anchor's scalar node. ok is false for a key that does not
// resolve to a scalar. Identity uses yaml.v3's own resolver (Decode) so
// equivalent spellings of one value -- `01`, `0x1`, and `1` as ints, `yes` and
// `true` as bools -- share an identity, while `"1"` (!!str) and `1` (!!int)
// stay distinct via the decoded Go type.
func resolveKeyIdent(node *yaml.Node) (keyIdent, bool) {
	target, ok := derefAlias(node)
	if !ok || target.Kind != yaml.ScalarNode {
		return keyIdent{}, false
	}
	var v interface{}
	if err := target.Decode(&v); err != nil {
		// Undecodable scalar: fall back to the raw spelling.
		return keyIdent{tag: target.Tag, value: target.Value}, true
	}
	return keyIdent{tag: fmt.Sprintf("%T", v), value: fmt.Sprintf("%v", v)}, true
}

// derefAlias follows an alias node to the node it points at, bounded by
// maxAliasHops. ok is false for a nil node or a dangling alias.
func derefAlias(node *yaml.Node) (*yaml.Node, bool) {
	for hops := 0; node != nil && node.Kind == yaml.AliasNode; hops++ {
		if node.Alias == nil || hops >= maxAliasHops {
			return nil, false
		}
		node = node.Alias
	}
	if node == nil {
		return nil, false
	}
	return node, true
}

// pathLabel renders a document path for an error message. The top level has no
// path of its own and is named `config`, matching the dotted style the nested
// paths (and dependency.go's messages) use.
func pathLabel(path string) string {
	if path == "" {
		return "config"
	}
	return path
}

// childPath extends a document path with one key.
func childPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
