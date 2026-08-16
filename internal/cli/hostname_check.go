package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/domain"
)

// docsBaseURL is the published docs site's base URL (zensical.toml's
// site_url). It is the one place in the Go code that spells that URL out —
// every doc link this package builds derives from it, so the base cannot
// drift between call sites the way two hand-typed URLs eventually do.
const docsBaseURL = "https://charliek.github.io/prox/"

// localDNSGuideURL is the local-DNS guide (docs/guides/local-dns.md), which
// explains why a `.test` (or similar non-resolving) domain needs local setup
// and what to do about it — see hostnameUnresolvedWarning.
const localDNSGuideURL = docsBaseURL + "guides/local-dns/"

// hostnameResolutionBudget bounds the WHOLE batch of hostname lookups this
// session runs at startup (see startHostnameResolutionCheck), not any single
// lookup. A resolver that is merely slow must never delay startup or produce
// a false "unresolvable" warning.
//
// It is deliberately SHORTER than warningProducerJoinTimeout rather than equal
// to it. At equal budgets a batch that answers correctly just before its own
// deadline still loses the race against the join -- there is real work after
// the last lookup (build the warning, Add it, Done the WaitGroup) that the
// join's timer does not wait for -- so the run that most needed the warning
// would be the one that dropped it (CodeRabbit review). The margin is what
// makes a slow-but-successful check still reach the terminal.
const hostnameResolutionBudget = 1500 * time.Millisecond

// hostnameResolver is the DNS lookup this package needs, narrowed to the one
// method so a test can inject a fake instead of hitting real DNS.
// *net.Resolver (net.DefaultResolver in production) already satisfies it.
type hostnameResolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// startHostnameResolutionCheck queues the async warning producer that checks
// whether hostnames resolve on this machine (issue #98: `prox up` printing a
// `.test` hostname that then NXDOMAINs in the user's browser). It runs via
// sink.Go so it overlaps startup instead of adding to it, and — like every
// producer registered with Go — it is joined (bounded by
// warningProducerJoinTimeout) before runUp renders and seals.
//
// Callers are both proxy-startup paths (tryDaemonProxy for the shared-daemon
// case, startProxy's standalone fallback), because the check has to be true
// regardless of which one the daemon-availability branch took (plan 028 B2
// design pin) — a hostname resolves or it doesn't independent of how prox
// got it registered.
//
// An empty hostnames list (proxy disabled, or --no-proxy) is a no-op: no
// sink.Go call, so no goroutine and no resolver call at all.
func startHostnameResolutionCheck(sink *warningSink, ctx context.Context, resolver hostnameResolver, hostnames []string) {
	// Deduplicate first. The shared-daemon path passes RegisterResponse.Registered
	// straight through, and the registry appends one entry per (service x
	// configured port) -- so a project with BOTH http_port and https_port set
	// (the dual-stack shape, and the one docs/guides/local-dns.md itself shows)
	// lists every hostname twice. Undeduped that means two lookups per name and
	// a warning that reads "app.sec.test, app.sec.test" (CodeRabbit review).
	// Doing it here rather than at one call site keeps the two paths identical.
	hostnames = uniqueStrings(hostnames)
	if len(hostnames) == 0 || resolver == nil {
		return
	}
	sink.Go(func() []domain.Warning {
		unresolved := checkHostnames(ctx, resolver, hostnames, hostnameResolutionBudget)
		if len(unresolved) == 0 {
			return nil
		}
		return []domain.Warning{hostnameUnresolvedWarning(unresolved)}
	})
}

// checkHostnames resolves every hostname concurrently against resolver,
// bounded by a single budget shared across the whole batch (not per-lookup),
// and returns the ones that did NOT resolve, in the same order they were
// given — never completion order, so the warning this feeds reads the same
// way on every run regardless of which lookup happens to answer first.
//
// It never fails: on any error, or if the batch does not finish inside
// budget, it treats that as "cannot tell" rather than "unresolvable" and
// returns nil. A resolver that is offline, sandboxed or merely slow must
// never turn into a false warning — the whole point is prox only speaking up
// when it actually knows the hostname will NXDOMAIN in a browser.
func checkHostnames(parent context.Context, resolver hostnameResolver, hostnames []string, budget time.Duration) []string {
	if len(hostnames) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()

	// notFound[i] is set ONLY when the resolver positively answered "no such
	// host". Success and every other kind of failure both leave it false, which
	// is the difference between "this will NXDOMAIN in your browser" and "I
	// could not find out" (CodeRabbit review).
	notFound := make([]bool, len(hostnames))
	var wg sync.WaitGroup
	wg.Add(len(hostnames))
	for i, h := range hostnames {
		go func(i int, h string) {
			defer wg.Done()
			_, err := resolver.LookupHost(ctx, h)
			notFound[i] = isHostNotFound(err)
		}(i, h)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Every lookup answered inside the budget; notFound[] is safe to read
		// — close(done) happens-after every write above.
	case <-ctx.Done():
		// The batch did not finish in time (or the caller's context was
		// cancelled). Some lookups may have completed and others may still be
		// running in the background, but a partial read of notFound here would produce a
		// warning keyed on which goroutines happened to lose the race rather
		// than on DNS truth — so report nothing rather than guess.
		return nil
	}

	var unresolved []string
	for i, missing := range notFound {
		if missing {
			unresolved = append(unresolved, hostnames[i])
		}
	}
	return unresolved
}

// isHostNotFound reports whether err is the resolver positively saying the name
// does not exist, as opposed to any other reason a lookup can fail.
//
// This distinction is the whole feature. Treating every error as "does not
// resolve" would mean a developer on a plane, behind a VPN, or in a
// network-sandboxed CI runner gets told their perfectly good `.test` or
// `lvh.me` setup is broken (CodeRabbit review) -- prox stating something false,
// which is exactly the class of bug plan 028 exists to remove. A timeout, an
// unreachable network or a misbehaving server all mean "I could not find out",
// and prox says nothing about what it could not find out.
func isHostNotFound(err error) bool {
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		return false
	}
	// IsNotFound is the authoritative NXDOMAIN/no-such-host answer. The other
	// two are explicitly NOT it, and are checked as belt and braces in case a
	// platform ever reports a timeout without setting IsNotFound false.
	return dnsErr.IsNotFound && !dnsErr.IsTimeout && !dnsErr.IsTemporary
}

// hostnameUnresolvedWarning builds the one warning naming every hostname
// that failed to resolve, in the order given (see checkHostnames).
func hostnameUnresolvedWarning(unresolved []string) domain.Warning {
	return domain.Warning{
		Code:    domain.WarningCodeHostnameUnresolved,
		Message: fmt.Sprintf("registered hostname(s) do not resolve on this machine: %s", strings.Join(unresolved, ", ")),
		Hint:    hostnameUnresolvedHint(unresolved),
	}
}

// hostnameUnresolvedHint builds the hint's "*.<domain> needs local DNS"
// clause from the unresolved hostnames themselves — never a hardcoded
// `*.test` — because the whole point of this warning is naming the ACTUAL
// configured domain (issue #98 was specifically about `.test` not
// resolving, but any project's domain can be in the same spot).
//
// A hostname is always `<service>.<domain>` (registry.go's construction), so
// the domain is everything after the first '.'. Domains are deduplicated and
// sorted for a stable message; the rare case where the unresolved set spans
// more than one domain (registered against multiple proxy domains, or a
// future multi-domain project) is handled by listing every `*.<domain>`
// wildcard rather than picking just one, with "need" instead of "needs" so
// the sentence still reads correctly.
func hostnameUnresolvedHint(unresolved []string) string {
	domains := uniqueHostDomains(unresolved)
	if len(domains) == 0 {
		return ""
	}
	wildcards := make([]string, len(domains))
	for i, d := range domains {
		wildcards[i] = "*." + d
	}
	verb := "needs"
	if len(wildcards) > 1 {
		verb = "need"
	}
	return fmt.Sprintf("%s %s local DNS — see %s", strings.Join(wildcards, ", "), verb, localDNSGuideURL)
}

// uniqueStrings returns in the same order, without duplicates.
func uniqueStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// uniqueHostDomains extracts the domain (everything after the first '.')
// from each `<service>.<domain>` hostname, returning the distinct set
// sorted. A hostname with no '.' is not one of ours (registry.go always
// produces svcName + "." + domain) and is skipped rather than guessed at.
func uniqueHostDomains(hostnames []string) []string {
	seen := make(map[string]struct{}, len(hostnames))
	var domains []string
	for _, h := range hostnames {
		idx := strings.IndexByte(h, '.')
		if idx < 0 || idx == len(h)-1 {
			continue
		}
		d := h[idx+1:]
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		domains = append(domains, d)
	}
	sort.Strings(domains)
	return domains
}

// defaultHostnameResolver is the production hostnameResolver: the standard
// library's resolver, which honors the context deadline checkHostnames gives
// it for every lookup.
var defaultHostnameResolver hostnameResolver = net.DefaultResolver

// registeredHostnames builds the standalone-mode hostname list with the
// SAME construction the daemon uses (registry.go: `svcName + "." +
// req.Domain`), so a standalone session gets the identical resolution check
// a shared-daemon session gets instead of this being a shared-mode-only
// feature (plan 028 B2 design pin). Sorted for a deterministic check order —
// cfg.Services is a map, so iteration order is not itself stable.
func registeredHostnames(cfg *config.Config) []string {
	if cfg == nil || cfg.Proxy == nil || cfg.Proxy.Domain == "" {
		return nil
	}
	hostnames := make([]string, 0, len(cfg.Services))
	for svcName := range cfg.Services {
		hostnames = append(hostnames, fmt.Sprintf("%s.%s", svcName, cfg.Proxy.Domain))
	}
	sort.Strings(hostnames)
	return hostnames
}
