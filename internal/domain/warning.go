package domain

// Warning is a single user-facing advisory produced somewhere the person who
// typed the command cannot see — most importantly inside the shared proxy
// daemon, whose stdout/stderr are /dev/null (internal/proxyd/daemon.go). It is
// the one warning shape on the wire: internal/proxyd and internal/api both use
// this type DIRECTLY rather than each defining a look-alike DTO plus a mapper,
// because two near-identical structs are exactly how Code and Hint wording
// drifts apart between the daemon that emits a warning and the CLI that prints
// it.
//
// Message and Hint are already user-facing prose, not error strings to be
// wrapped: whatever produces a Warning owns the wording, and the CLI prints it
// verbatim.
type Warning struct {
	// Code is a stable machine identifier for the warning kind (e.g.
	// WarningCodeMkcertCAUntrusted). It never changes wording, so clients and
	// tests can key off it; use the constants below rather than literals.
	Code string `json:"code"`
	// Message is one or more complete, user-facing sentences describing what is
	// wrong.
	Message string `json:"message"`
	// Hint is the next action the user should take, when there is one.
	Hint string `json:"hint,omitempty"`
}

// Warning codes. Both the producer (the daemon's cert layer) and the consumer
// (the CLI) reference these constants so a warning cannot be emitted under one
// spelling and matched under another.
const (
	// WarningCodeMkcertCAUntrusted means mkcert generated certificates but its
	// local CA is not installed in the system/browser trust stores, so HTTPS
	// through the proxy will show certificate errors until `mkcert -install`
	// runs.
	WarningCodeMkcertCAUntrusted = "mkcert_ca_untrusted"

	// WarningCodeHostnameUnresolved means one or more of this session's
	// registered `<service>.<domain>` hostnames did not resolve via DNS on
	// this machine — e.g. a `.test` domain, which does not resolve without
	// local setup, as opposed to a public wildcard like `*.lvh.me`. Pasting
	// such a hostname into a browser gets NXDOMAIN even though prox itself is
	// listening (issue #98).
	WarningCodeHostnameUnresolved = "hostname_unresolved"
)

// DedupeWarnings returns ws with duplicates (same Code AND Message) removed,
// preserving first-seen order. Hint is deliberately NOT part of the identity:
// two warnings that say the same thing are one warning to the user even if a
// producer attached a hint to only one of them, and the first-seen copy (hint
// and all) is the one kept.
//
// It is safe on nil and empty input, returning nil so an omitempty JSON field
// disappears rather than serializing as [].
func DedupeWarnings(ws []Warning) []Warning {
	if len(ws) == 0 {
		return nil
	}
	type key struct{ code, message string }
	seen := make(map[key]struct{}, len(ws))
	out := make([]Warning, 0, len(ws))
	for _, w := range ws {
		k := key{w.Code, w.Message}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, w)
	}
	return out
}
