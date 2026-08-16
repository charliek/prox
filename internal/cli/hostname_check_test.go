package cli

import (
	"context"
	"errors"
	"net"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file covers the hostname-resolution check (plan 028 B2, #98): `prox up`
// prints `Registered domains: app.sec.test`, which NXDOMAINs in a browser
// because `.test` does not resolve without local DNS setup, and nothing warns
// the user at the moment it matters. No test here may perform a real DNS
// lookup — every resolver is a fake.

// fakeResolver answers LookupHost from a fixed set of resolvable hostnames,
// optionally releasing answers out of completion order via release channels
// keyed by hostname, and records the hostnames it was actually asked to look
// up (mutex-guarded — checkHostnames calls it concurrently).
type fakeResolver struct {
	// forcedErr, when set, is returned for every lookup instead of the
	// resolvable-map answer — so a test can model a failure that is NOT
	// NXDOMAIN.
	forcedErr error

	resolvable map[string]bool

	mu      sync.Mutex
	release map[string]chan struct{} // if set for a host, LookupHost blocks on it
	called  []string

	hang bool // if true, every lookup blocks until ctx is done and stays hung after
}

func (f *fakeResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	if f.forcedErr != nil {
		f.mu.Lock()
		f.called = append(f.called, host)
		f.mu.Unlock()
		return nil, f.forcedErr
	}
	f.mu.Lock()
	f.called = append(f.called, host)
	ch := f.release[host]
	f.mu.Unlock()

	if f.hang {
		// A resolver that does not honor context cancellation at all — the
		// "truly hung" case checkHostnames must survive without hanging
		// itself or reporting a false positive.
		select {}
	}
	if ch != nil {
		<-ch
	}

	if f.resolvable[host] {
		return []string{"127.0.0.1"}, nil
	}
	// A REAL *net.DNSError with IsNotFound, not a bare error: prox only warns
	// on a positive "no such host", so a fake returning something generic
	// would be testing a code path production can never take.
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func (f *fakeResolver) calledHosts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.called))
	copy(out, f.called)
	return out
}

// TestCheckHostnames_AllResolve: nothing unresolved, nothing to warn about.
func TestCheckHostnames_AllResolve(t *testing.T) {
	r := &fakeResolver{resolvable: map[string]bool{"app.sec.test": true, "api.sec.test": true}}
	got := checkHostnames(context.Background(), r, []string{"app.sec.test", "api.sec.test"}, time.Second)
	assert.Empty(t, got)
}

// TestCheckHostnames_SomeFail_OrderIsInputOrderNotCompletionOrder: the fake
// deliberately releases hostnames in the REVERSE of the order they were
// given, so a result ordered by completion would come back backwards. The
// warning must instead read in input order every time (deterministic across
// runs, not a race between goroutines).
func TestCheckHostnames_SomeFail_OrderIsInputOrderNotCompletionOrder(t *testing.T) {
	hostnames := []string{"app.sec.test", "api.sec.test", "web.sec.test"}
	release := map[string]chan struct{}{
		"app.sec.test": make(chan struct{}),
		"api.sec.test": make(chan struct{}),
		"web.sec.test": make(chan struct{}),
	}
	r := &fakeResolver{
		resolvable: map[string]bool{}, // none resolve
		release:    release,
	}

	done := make(chan []string, 1)
	go func() {
		done <- checkHostnames(context.Background(), r, hostnames, 2*time.Second)
	}()

	// Release in reverse of input order, with a small stagger so completion
	// order is provably the opposite of input order.
	close(release["web.sec.test"])
	time.Sleep(10 * time.Millisecond)
	close(release["api.sec.test"])
	time.Sleep(10 * time.Millisecond)
	close(release["app.sec.test"])

	got := <-done
	assert.Equal(t, hostnames, got, "unresolved hosts must come back in INPUT order, not completion order")
}

// TestCheckHostnames_AllFail: one warning naming all of them, still in order.
func TestCheckHostnames_AllFail(t *testing.T) {
	hostnames := []string{"app.sec.test", "api.sec.test"}
	r := &fakeResolver{resolvable: map[string]bool{}}
	got := checkHostnames(context.Background(), r, hostnames, time.Second)
	assert.Equal(t, hostnames, got)
}

// TestCheckHostnames_ResolverHangs_BoundedByBudget_NoWarning: a resolver that
// never returns (does not even honor context cancellation) must not hang the
// caller past the injected budget, and must produce NO result (not "all
// failed") — silence is the correct answer for "cannot tell", not a false
// positive.
func TestCheckHostnames_ResolverHangs_BoundedByBudget_NoWarning(t *testing.T) {
	r := &fakeResolver{hang: true}
	hostnames := []string{"app.sec.test", "api.sec.test"}

	start := time.Now()
	budget := 50 * time.Millisecond
	got := checkHostnames(context.Background(), r, hostnames, budget)
	elapsed := time.Since(start)

	assert.Nil(t, got, "a timed-out batch must report nothing, not a false 'all unresolved'")
	assert.Less(t, elapsed, 2*time.Second, "must be bounded by the injected budget, not some larger default")
}

// TestCheckHostnames_Empty_NoWarningNoResolverCalls: nothing to check, and
// critically, the resolver is never even called — confirmed via
// startHostnameResolutionCheck, which must not queue a producer at all.
func TestCheckHostnames_Empty_NoWarningNoResolverCalls(t *testing.T) {
	r := &fakeResolver{resolvable: map[string]bool{}}
	got := checkHostnames(context.Background(), r, nil, time.Second)
	assert.Nil(t, got)
	assert.Empty(t, r.calledHosts())

	sink := newWarningSink()
	startHostnameResolutionCheck(sink, context.Background(), r, nil)
	require.True(t, sink.Wait(time.Second), "no producer should have been queued for an empty hostname list")
	assert.Empty(t, sink.Warnings())
	assert.Empty(t, r.calledHosts())
}

// TestStartHostnameResolutionCheck_AllResolve_NoWarning exercises the full
// sink.Go wiring end to end with a resolver that answers everything.
func TestStartHostnameResolutionCheck_AllResolve_NoWarning(t *testing.T) {
	r := &fakeResolver{resolvable: map[string]bool{"app.sec.test": true}}
	sink := newWarningSink()
	startHostnameResolutionCheck(sink, context.Background(), r, []string{"app.sec.test"})
	require.True(t, sink.Wait(time.Second))
	assert.Empty(t, sink.Warnings())
}

// TestHostnameUnresolvedWarning_HintCarriesActualDomainAndDocsURL: the hint
// must name the ACTUAL configured domain (never a hardcoded `*.test`) and the
// docs URL must be built from the one docsBaseURL constant, not typed out
// again.
func TestHostnameUnresolvedWarning_HintCarriesActualDomainAndDocsURL(t *testing.T) {
	w := hostnameUnresolvedWarning([]string{"app.sec.test", "api.sec.test"})

	assert.Equal(t, "hostname_unresolved", w.Code)
	assert.Equal(t, "registered hostname(s) do not resolve on this machine: app.sec.test, api.sec.test", w.Message)
	assert.Contains(t, w.Hint, "*.sec.test needs local DNS")
	assert.Contains(t, w.Hint, docsBaseURL+"guides/local-dns/")
	assert.Equal(t, localDNSGuideURL, docsBaseURL+"guides/local-dns/")
}

// TestHostnameUnresolvedWarning_MultipleDomains: hostnames spanning more than
// one domain get every wildcard named, pluralized correctly, rather than
// silently picking one and dropping the other.
func TestHostnameUnresolvedWarning_MultipleDomains(t *testing.T) {
	w := hostnameUnresolvedWarning([]string{"app.sec.test", "api.other.dev"})
	assert.Contains(t, w.Hint, "*.other.dev")
	assert.Contains(t, w.Hint, "*.sec.test")
	assert.Contains(t, w.Hint, " need local DNS", "plural verb when more than one domain is involved")
}

// TestRegisteredHostnames_MatchesDaemonConstruction: standalone mode must
// derive `<service>.<domain>` the SAME way the daemon does
// (registry.go:~152's `fmt.Sprintf("%s.%s", svcName, req.Domain)`), or the
// check would silently only run in shared-daemon mode.
func TestRegisteredHostnames_MatchesDaemonConstruction(t *testing.T) {
	cfg := &config.Config{
		Proxy: &config.ProxyConfig{Enabled: true, Domain: "sec.test"},
		Services: map[string]config.ServiceConfig{
			"app": {Port: 3000},
			"api": {Port: 4000},
		},
	}
	got := registeredHostnames(cfg)
	want := []string{"api.sec.test", "app.sec.test"}
	sort.Strings(got)
	assert.Equal(t, want, got)
}

// TestRegisteredHostnames_NoProxyDomain_Empty: no proxy configured (or no
// domain set) must never synthesize hostnames out of nothing.
func TestRegisteredHostnames_NoProxyDomain_Empty(t *testing.T) {
	assert.Empty(t, registeredHostnames(&config.Config{}))
	assert.Empty(t, registeredHostnames(nil))
}

// TestCheckHostnames_AmbiguousErrorsNeverWarn is the anti-false-alarm test.
//
// A lookup can fail for reasons that say nothing about the hostname: a laptop
// on a plane, a VPN-only network, a sandboxed CI runner with no resolver. If
// prox treated those as "does not resolve" it would confidently tell a user
// their perfectly good setup was broken — the same class of untruth as the
// silence this feature replaces, pointed the other way. Only a positive
// NXDOMAIN counts.
func TestCheckHostnames_AmbiguousErrorsNeverWarn(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"network unreachable", &net.DNSError{Err: "connect: network is unreachable", Name: "app.sec.test"}},
		{"timeout", &net.DNSError{Err: "i/o timeout", Name: "app.sec.test", IsTimeout: true}},
		{"temporary server failure", &net.DNSError{Err: "server misbehaving", Name: "app.sec.test", IsTemporary: true}},
		{"not a DNS error at all", errors.New("something else entirely")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeResolver{resolvable: map[string]bool{}, forcedErr: tc.err}
			got := checkHostnames(context.Background(), r, []string{"app.sec.test"}, time.Second)
			assert.Empty(t, got,
				"a lookup that failed for a reason other than NXDOMAIN means "+
					"'I could not find out', and prox must not warn about what it does not know")
		})
	}
}

// TestStartHostnameResolutionCheck_DedupesHostnames pins the dual-stack shape.
//
// The registry appends one entry per (service x configured port), so a project
// with both http_port and https_port — the shape docs/guides/local-dns.md
// itself shows — reports every hostname twice in RegisterResponse.Registered.
// Undeduped that is two lookups per name and a warning reading
// "app.sec.test, app.sec.test".
func TestStartHostnameResolutionCheck_DedupesHostnames(t *testing.T) {
	r := &fakeResolver{resolvable: map[string]bool{}}
	sink := newWarningSink()

	startHostnameResolutionCheck(sink, context.Background(), r,
		[]string{"app.sec.test", "api.sec.test", "app.sec.test", "api.sec.test"})
	require.True(t, sink.Wait(5*time.Second))

	assert.ElementsMatch(t, []string{"app.sec.test", "api.sec.test"}, r.calledHosts(),
		"each hostname is looked up once, however many ports registered it")

	ws := sink.Warnings()
	require.Len(t, ws, 1)
	assert.Equal(t,
		"registered hostname(s) do not resolve on this machine: app.sec.test, api.sec.test",
		ws[0].Message, "no hostname may appear twice in the sentence")
}
