package proxyd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy/certs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testWarning is the stand-in for the warning B1's mkcert detection will
// produce. It uses the shared code constant so this test cannot drift from the
// producer's spelling.
var testWarning = domain.Warning{
	Code:    domain.WarningCodeMkcertCAUntrusted,
	Message: "The mkcert local CA is not installed in your trust stores, so HTTPS URLs will show certificate errors.",
	Hint:    "Run `mkcert -install`, then restart your browser.",
}

// registerResp drives a register to success and returns the typed success body.
// Like registerOK it tolerates the transient PORT_BIND_FAILED a fresh-port bind
// can hit when the OS briefly holds a just-closed ephemeral port; unlike it, the
// response body is the point of the call.
func registerResp(t *testing.T, s *Server, req RegisterRequest) RegisterResponse {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, body := s.register(req)
		if status == http.StatusOK {
			resp, ok := body.(RegisterResponse)
			require.True(t, ok, "success body should be a RegisterResponse, got %T", body)
			return resp
		}
		if time.Now().After(deadline) {
			t.Fatalf("register never succeeded: status=%d body=%v", status, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRegisterResponse_WarningsWireRoundTrip pins the wire contract of the new
// field: it round-trips losslessly, it disappears entirely when there are no
// warnings (omitempty), and an older daemon's payload — which has no "warnings"
// key at all — still decodes cleanly to nil. That last case is the whole
// compatibility argument: the version gate does NOT separate two "dev" builds,
// so a new client really can meet an old daemon's payload.
func TestRegisterResponse_WarningsWireRoundTrip(t *testing.T) {
	t.Run("round-trips losslessly", func(t *testing.T) {
		in := RegisterResponse{
			Registered: []string{"api.local.dev", "web.local.dev"},
			Warnings: []domain.Warning{
				testWarning,
				{Code: "other", Message: "Something else."},
			},
		}
		raw, err := json.Marshal(in)
		require.NoError(t, err)

		var out RegisterResponse
		require.NoError(t, json.Unmarshal(raw, &out))
		assert.Equal(t, in, out)
	})

	t.Run("omits the key when there are no warnings", func(t *testing.T) {
		raw, err := json.Marshal(RegisterResponse{Registered: []string{"api.local.dev"}})
		require.NoError(t, err)
		assert.JSONEq(t, `{"registered":["api.local.dev"]}`, string(raw))

		raw, err = json.Marshal(RegisterResponse{Registered: []string{"api.local.dev"}, Warnings: []domain.Warning{}})
		require.NoError(t, err)
		assert.JSONEq(t, `{"registered":["api.local.dev"]}`, string(raw),
			"an empty (non-nil) slice must also omit the key")
	})

	t.Run("older daemon payload decodes to nil warnings", func(t *testing.T) {
		var out RegisterResponse
		require.NoError(t, json.Unmarshal([]byte(`{"registered":["api.local.dev"]}`), &out))
		assert.Equal(t, []string{"api.local.dev"}, out.Registered)
		assert.Nil(t, out.Warnings)
	})
}

// TestRegister_NormalArmReturnsWarnings pins that the ordinary success arm of
// register() carries the daemon's recorded warnings back to the CLI.
func TestRegister_NormalArmReturnsWarnings(t *testing.T) {
	backendHost, backendPort := backendTarget(t)
	fake := newFakeCertManager()
	s := newProxyServerWithCertMgr(t, fake)

	// The producer seam: stand in for B1's detection during cert preparation.
	fake.RecordWarning(testWarning)

	resp := registerResp(t, s, RegisterRequest{
		ProjectDir: "/projects/normal", PID: os.Getpid(), Version: "test", Domain: "normal.test",
		Services:  map[string]ServiceTarget{"svc": {Host: backendHost, Port: backendPort}},
		HTTPSPort: freePort(t),
	})

	assert.Equal(t, []string{"svc.normal.test"}, resp.Registered)
	assert.Equal(t, []domain.Warning{testWarning}, resp.Warnings,
		"the normal register arm must return the daemon's warnings")
}

// TestRegister_NoOpReRegisterReturnsWarnings is the regression this commit is
// most likely to lose: the idempotent no-op refresh arm returns BEFORE the cert
// phase, so it is the easiest success return to forget. It is also the self-heal
// path (cli.proxy_runtime), i.e. precisely the client that has not yet seen the
// warning.
//
// It drives a REAL second registration with an unchanged config so
// registrationMatches takes that branch, and proves the branch was taken by the
// cert manager's EnsureDomain count staying at one — the config-changed arm would
// have re-run the cert phase.
func TestRegister_NoOpReRegisterReturnsWarnings(t *testing.T) {
	backendHost, backendPort := backendTarget(t)
	fake := newFakeCertManager()
	s := newProxyServerWithCertMgr(t, fake)
	pid, token := liveIdentity(t)

	fake.RecordWarning(testWarning)

	req := RegisterRequest{
		ProjectDir: "/projects/noop", PID: pid, StartTime: token, Version: "test", Domain: "noop.test",
		Services:  map[string]ServiceTarget{"svc": {Host: backendHost, Port: backendPort}},
		HTTPSPort: freePort(t),
	}
	registerOK(t, s, req)
	// A BASELINE, not a literal 1: registerOK retries a transient
	// PORT_BIND_FAILED, and the cert phase runs before the listener bind, so a
	// retried first register legitimately leaves the count above one. What the
	// assertion below needs is only that the SECOND register adds nothing.
	baseline := fake.ensureCount("noop.test")
	require.GreaterOrEqual(t, baseline, 1, "precondition: the first register ran the cert phase")

	// Same identity, same config: a true no-op refresh.
	resp := registerResp(t, s, req)

	require.Equal(t, baseline, fake.ensureCount("noop.test"),
		"the second register must have taken the no-op refresh arm (which never reaches the cert phase)")
	assert.Equal(t, []string{"svc.noop.test"}, resp.Registered)
	assert.Equal(t, []domain.Warning{testWarning}, resp.Warnings,
		"the no-op refresh arm must still report the daemon's warnings")
}

// TestRegister_WarningsSurviveWarmCertGeneration pins the CA-scoped cache: a
// registration that generates NOTHING (the cert for its domain is already
// loaded — the warm-certs case a restarted daemon lives in) still gets the
// warning recorded by the one generation that did run. A per-domain or
// per-generation store would silently drop it here, reverting to today's
// swallowed-warning bug.
//
// It uses the real MultiDomainCertManager (real holder, real EnsureDomain cache)
// with only the generator replaced by a synthetic producer that records the
// warning the way B1's detection will.
func TestRegister_WarningsSurviveWarmCertGeneration(t *testing.T) {
	backendHost, backendPort := backendTarget(t)

	m := NewMultiDomainCertManager(t.TempDir())
	var generated atomic.Int32
	m.generate = func(d string) (*tls.Certificate, error) {
		generated.Add(1)
		m.RecordWarning(testWarning) // the producer seam, fired during generation
		return generateWildcardCert(d)
	}
	// The real resolver is left in place: nothing in this test generates a
	// certificate, so its verdict is UNKNOWN and applyCATrust touches the holder
	// neither way, leaving this test's synthetic RecordWarning to stand for the
	// producer.
	s := newProxyServerWithCertMgr(t, m)
	port := freePort(t)

	respA := registerResp(t, s, RegisterRequest{
		ProjectDir: "/projects/a", PID: os.Getpid(), Version: "test", Domain: "warm.test",
		Services:  map[string]ServiceTarget{"a": {Host: backendHost, Port: backendPort}},
		HTTPSPort: port,
	})
	require.EqualValues(t, 1, generated.Load(), "precondition: the first register generated the cert")
	require.Equal(t, []domain.Warning{testWarning}, respA.Warnings)

	// A second project on the SAME base domain: EnsureDomain hits its loaded-cert
	// fast path, so the producer never fires again.
	respB := registerResp(t, s, RegisterRequest{
		ProjectDir: "/projects/b", PID: os.Getpid(), Version: "test", Domain: "warm.test",
		Services:  map[string]ServiceTarget{"b": {Host: backendHost, Port: backendPort}},
		HTTPSPort: port,
	})

	assert.EqualValues(t, 1, generated.Load(), "precondition: the second register generated nothing")
	assert.Equal(t, []domain.Warning{testWarning}, respB.Warnings,
		"a registration that generated no certs must still report the CA-scoped warning")
}

// TestRegister_DedupesWarnings pins that a condition observed several times (the
// producer runs per domain, the CA state is ONE state) reaches the user once.
//
// The holder is keyed by code, so repeated observations of the same code
// collapse at the point of RECORDING rather than only at the point of reading —
// which is what keeps it bounded no matter how often a producer speaks. A later
// observation replaces the earlier wording; distinct codes are distinct
// warnings.
func TestRegister_DedupesWarnings(t *testing.T) {
	backendHost, backendPort := backendTarget(t)
	fake := newFakeCertManager()
	s := newProxyServerWithCertMgr(t, fake)

	fake.RecordWarning(testWarning)
	fake.RecordWarning(testWarning)
	// Same code, revised wording: the latest verdict wins rather than stacking.
	revised := domain.Warning{Code: domain.WarningCodeMkcertCAUntrusted, Message: "A different message."}
	fake.RecordWarning(revised)
	// A genuinely different code is a genuinely different warning.
	unrelated := domain.Warning{Code: "some_other_condition", Message: "Unrelated."}
	fake.RecordWarning(unrelated)

	resp := registerResp(t, s, RegisterRequest{
		ProjectDir: "/projects/dupe", PID: os.Getpid(), Version: "test", Domain: "dupe.test",
		Services:  map[string]ServiceTarget{"svc": {Host: backendHost, Port: backendPort}},
		HTTPSPort: freePort(t),
	})

	assert.Equal(t, []domain.Warning{revised, unrelated}, resp.Warnings)
}

// TestRegister_NoWarningsOmitsField pins the clean case: with nothing recorded,
// the response carries no warnings at all (so the key is absent on the wire), and
// a daemon with no cert manager at all does not panic reaching for them.
func TestRegister_NoWarningsOmitsField(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(http.ResponseWriter, *http.Request) {})

	t.Run("cert manager with nothing recorded", func(t *testing.T) {
		fake := newFakeCertManager()
		s := newProxyServerWithCertMgr(t, fake)
		resp := registerResp(t, s, RegisterRequest{
			ProjectDir: "/projects/quiet", PID: os.Getpid(), Version: "test", Domain: "quiet.test",
			Services:  map[string]ServiceTarget{"svc": {Host: backendHost, Port: backendPort}},
			HTTPSPort: freePort(t),
		})
		assert.Empty(t, resp.Warnings)
	})

	t.Run("no cert manager at all", func(t *testing.T) {
		s := newProxyServer(t) // wired with a nil certMgr
		resp := registerResp(t, s, RegisterRequest{
			ProjectDir: "/projects/nocert", PID: os.Getpid(), Version: "test", Domain: "nocert.test",
			Services: map[string]ServiceTarget{"svc": {Host: backendHost, Port: backendPort}},
			HTTPPort: freePort(t),
		})
		assert.Nil(t, resp.Warnings)
	})
}

// TestClientRegister_PassesWarningsThrough closes the loop over the real
// transport: a warning recorded in the daemon must reach a Client.Register
// caller unchanged, having actually gone through JSON and the Unix socket. This
// commit adds no CLI-side consumption (that is A2) — this only pins that the
// field survives the trip.
func TestClientRegister_PassesWarningsThrough(t *testing.T) {
	server, client, _ := startTestServer(t)

	// startTestServer wires no proxy; give it one with a fake cert manager so the
	// daemon has somewhere to hold warnings.
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	fake := newFakeCertManager()
	dp := NewDynamicProxy(server.registry, fake, server.managers, nil, logger)
	server.SetProxy(dp)
	t.Cleanup(func() { _ = dp.Shutdown(context.Background()) })

	fake.RecordWarning(testWarning)

	resp, err := client.Register(RegisterRequest{
		ProjectDir: "/projects/wire", PID: os.Getpid(), Version: "test-version", Domain: "wire.test",
		Services: map[string]ServiceTarget{"svc": {Host: "127.0.0.1", Port: 3000}},
		HTTPPort: freePort(t),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"svc.wire.test"}, resp.Registered)
	assert.Equal(t, []domain.Warning{testWarning}, resp.Warnings,
		"the client must return the daemon's warnings to its caller unchanged")
}

// TestCertWarningHolder_Concurrent is the -race guard for the holder: recording
// and reading run concurrently (as the cert layer and register() can), and the
// snapshot must be a copy the caller can mutate without corrupting the holder.
func TestCertWarningHolder_Concurrent(t *testing.T) {
	m := NewMultiDomainCertManager(t.TempDir())
	// Never shell to mkcert from a test: swap in the in-memory generator. The
	// trust verdict is left real — nothing here generates a certificate, so it
	// stays UNKNOWN and leaves this test's own RecordWarning standing, which is
	// what it is measuring.
	m.generate = func(d string) (*tls.Certificate, error) { return generateWildcardCert(d) }
	backendHost, backendPort := backendTarget(t)
	s := newProxyServerWithCertMgr(t, m)

	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); <-start; m.RecordWarning(testWarning) }()
		go func() { defer wg.Done(); <-start; _ = m.Warnings() }()
		go func() { defer wg.Done(); <-start; _ = s.currentWarnings() }()
	}
	close(start)
	wg.Wait()

	// Deduped, the n identical records are one warning.
	assert.Equal(t, []domain.Warning{testWarning}, s.currentWarnings())

	// The snapshot is a copy: mutating it must not corrupt the holder.
	snap := m.Warnings()
	require.NotEmpty(t, snap)
	snap[0] = domain.Warning{Code: "mutated", Message: "mutated"}
	assert.Equal(t, testWarning, m.Warnings()[0], "Warnings must hand out a copy")

	// And a register concurrent-with-nothing still reports the deduped set.
	resp := registerResp(t, s, RegisterRequest{
		ProjectDir: "/projects/race", PID: os.Getpid(), Version: "test", Domain: "race.test",
		Services:  map[string]ServiceTarget{"svc": {Host: backendHost, Port: backendPort}},
		HTTPSPort: freePort(t),
	})
	assert.Equal(t, []domain.Warning{testWarning}, resp.Warnings)
}

// TestCertWarningHolder_SetIsIdempotentPerCode pins that the holder is keyed by
// code rather than append-only. A producer that observes the same condition
// while preparing several base domains must yield ONE warning, not one per
// domain, and the holder must stay bounded by the number of distinct codes
// however many times it is told (CodeRabbit review, N2).
func TestCertWarningHolder_SetIsIdempotentPerCode(t *testing.T) {
	var h certWarningHolder
	for range 50 {
		h.set(testWarning)
	}
	require.Len(t, h.snapshot(), 1, "same code recorded 50 times is still one warning")

	// A later observation of the same code replaces the wording rather than
	// stacking a second copy.
	updated := testWarning
	updated.Message = "revised wording"
	h.set(updated)
	got := h.snapshot()
	require.Len(t, got, 1)
	assert.Equal(t, "revised wording", got[0].Message)

	// A different code is a different warning, and insertion order holds.
	h.set(domain.Warning{Code: "other", Message: "second"})
	got = h.snapshot()
	require.Len(t, got, 2)
	assert.Equal(t, domain.WarningCodeMkcertCAUntrusted, got[0].Code)
	assert.Equal(t, "other", got[1].Code)
}

// TestCertWarningHolder_ClearWithdrawsAResolvedWarning is the anti-lying test.
//
// The condition these warnings describe is one the user goes and FIXES. A
// verdict that could only ever be added would keep reporting an untrusted CA
// for the rest of a long-lived daemon's life after the user ran
// `mkcert -install` — telling them something false, which is the exact bug
// plan 028 exists to remove (CodeRabbit review, N1).
func TestCertWarningHolder_ClearWithdrawsAResolvedWarning(t *testing.T) {
	var h certWarningHolder
	h.set(testWarning)
	h.set(domain.Warning{Code: "other", Message: "second"})
	require.Len(t, h.snapshot(), 2)

	h.clear(testWarning.Code)
	got := h.snapshot()
	require.Len(t, got, 1, "the cleared warning is gone")
	assert.Equal(t, "other", got[0].Code, "and the survivor is untouched")

	// Clearing something absent is a no-op, and clearing everything returns nil
	// (not an empty slice) so the omitempty wire field disappears.
	h.clear("never-recorded")
	h.clear("other")
	assert.Nil(t, h.snapshot(), "an emptied holder reports nil, so the JSON key vanishes")

	// It can be re-recorded afterwards: the condition can come back.
	h.set(testWarning)
	require.Len(t, h.snapshot(), 1)
}

// TestRegister_ClearedWarningStopsBeingReported drives the same lifecycle
// through the wire: a warning the daemon has withdrawn must not keep arriving
// on later registrations.
func TestRegister_ClearedWarningStopsBeingReported(t *testing.T) {
	backendHost, backendPort := backendTarget(t)
	fake := newFakeCertManager()
	s := newProxyServerWithCertMgr(t, fake)
	pid, token := liveIdentity(t)
	fake.RecordWarning(testWarning)

	req := RegisterRequest{
		ProjectDir: "/projects/cleared", PID: pid, StartTime: token, Version: "test",
		Domain:    "cleared.test",
		Services:  map[string]ServiceTarget{"svc": {Host: backendHost, Port: backendPort}},
		HTTPSPort: freePort(t),
	}
	resp := registerResp(t, s, req)
	require.Len(t, resp.Warnings, 1, "precondition: the warning is being reported")

	// The user fixed it; the producer withdraws the verdict.
	fake.ClearWarning(testWarning.Code)

	resp = registerResp(t, s, req)
	assert.Empty(t, resp.Warnings,
		"a withdrawn warning must not keep arriving — reporting a fixed problem is the bug, not the fix")
}

// warmCertFiles writes a usable cert/key pair to dir under the names
// certs.Manager expects, standing in for "the certs are already on disk". It is
// the state a daemon restarted onto a new release starts in — the one in which
// mkcert never runs and therefore never says anything.
func warmCertFiles(t *testing.T, dir, baseDomain string) {
	t.Helper()
	cert, err := generateWildcardCert(baseDomain)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)

	safe := strings.ReplaceAll(baseDomain, ".", "_")
	require.NoError(t, os.WriteFile(filepath.Join(dir, safe+".pem"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, safe+"-key.pem"),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
}

// warmCertManager returns a real MultiDomainCertManager (real holder, real
// EnsureDomain cache, real certs.Manager) whose certs are already on disk and
// whose CA-trust verdict is the given one. Only the verdict is faked: what
// mkcert's output MEANS is tested where it is parsed (internal/proxy/certs);
// what the DAEMON does with the verdict is what these tests pin.
func warmCertManager(t *testing.T, baseDomain string, v certs.TrustVerdict) *MultiDomainCertManager {
	t.Helper()
	dir := t.TempDir()
	warmCertFiles(t, dir, baseDomain)
	m := NewMultiDomainCertManager(dir)
	m.resolveTrust = func() certs.TrustVerdict { return v }
	return m
}

// TestEnsureDomain_ColdGenerationWarnsOnFirstRegistration pins the ordering
// CodeRabbit caught on PR #111: the verdict read at the top of EnsureDomain
// runs BEFORE generation, so on a cold-cert path it sees Unknown and records
// nothing — the generation itself is what first learns the CA is untrusted.
// EnsureDomain must re-apply the verdict after a successful generation, or the
// registration that just generated returns without the warning and the user
// learns about their broken CA one `prox up` too late.
func TestEnsureDomain_ColdGenerationWarnsOnFirstRegistration(t *testing.T) {
	untrusted := testWarning
	m := NewMultiDomainCertManager(t.TempDir())

	// The verdict flips from Unknown to untrusted only when generation runs,
	// exactly as a real mkcert run flips the shared resolver via observe.
	verdict := certs.TrustVerdict{}
	m.resolveTrust = func() certs.TrustVerdict { return verdict }
	m.generate = func(domain string) (*tls.Certificate, error) {
		verdict = certs.TrustVerdict{Known: true, Warning: &untrusted}
		return generateWildcardCert(domain)
	}

	require.NoError(t, m.EnsureDomain("coldwarn.test"))
	assert.Equal(t, []domain.Warning{untrusted}, m.Warnings(),
		"the registration whose own generation discovered the untrusted CA must report it, not defer it to the next one")
}

// TestGenerateViaMkcert_PublishesWarmCertVerdict is the case the daemon exists
// to cover: certs already on disk, mkcert never runs, and the warning still
// reaches the client: the verdict a real generation recorded earlier in this
// process is replayed on every registration, so a warm cert load still publishes
// it.
func TestGenerateViaMkcert_PublishesWarmCertVerdict(t *testing.T) {
	untrusted := testWarning
	m := warmCertManager(t, "warmwarn.test", certs.TrustVerdict{Known: true, Warning: &untrusted})

	require.NoError(t, m.EnsureDomain("warmwarn.test"))
	assert.Equal(t, []domain.Warning{untrusted}, m.Warnings(),
		"a generation-free cert load must still publish the CA-scoped verdict")
}

// TestGenerateViaMkcert_TrustedVerdictWithdrawsWarning is the anti-lying half
// wired end to end: the user ran `mkcert -install`, so the daemon must stop
// saying otherwise rather than repeating a fixed problem until it restarts.
func TestGenerateViaMkcert_TrustedVerdictWithdrawsWarning(t *testing.T) {
	m := warmCertManager(t, "fixed.test", certs.TrustVerdict{Known: true})
	m.RecordWarning(testWarning)
	require.Len(t, m.Warnings(), 1, "precondition: the warning is being reported")

	require.NoError(t, m.EnsureDomain("fixed.test"))
	assert.Empty(t, m.Warnings(), "a trusted verdict must withdraw the warning")
}

// TestGenerateViaMkcert_UnknownVerdictChangesNothing pins the third state. An
// unknown verdict (nothing has generated a certificate in this process yet) is
// not evidence of trust, so it must not retract a warning, and not evidence of
// a problem, so it must not raise one.
func TestGenerateViaMkcert_UnknownVerdictChangesNothing(t *testing.T) {
	m := warmCertManager(t, "unknown.test", certs.TrustVerdict{})
	m.RecordWarning(testWarning)

	require.NoError(t, m.EnsureDomain("unknown.test"))
	assert.Equal(t, []domain.Warning{testWarning}, m.Warnings(),
		"an unknown verdict must neither raise nor retract anything")
}

// TestRegister_ReportsCATrustWarningFromCertPhase closes the daemon loop over a
// real register: the cert phase itself — not a test calling RecordWarning —
// produces the warning the response carries.
func TestRegister_ReportsCATrustWarningFromCertPhase(t *testing.T) {
	backendHost, backendPort := backendTarget(t)
	untrusted := testWarning
	m := warmCertManager(t, "catrust.test", certs.TrustVerdict{Known: true, Warning: &untrusted})
	s := newProxyServerWithCertMgr(t, m)

	resp := registerResp(t, s, RegisterRequest{
		ProjectDir: "/projects/catrust", PID: os.Getpid(), Version: "test", Domain: "catrust.test",
		Services:  map[string]ServiceTarget{"svc": {Host: backendHost, Port: backendPort}},
		HTTPSPort: freePort(t),
	})

	assert.Equal(t, []string{"svc.catrust.test"}, resp.Registered)
	assert.Equal(t, []domain.Warning{untrusted}, resp.Warnings,
		"the cert phase's own verdict must reach the client that cannot see the daemon's output")
}
