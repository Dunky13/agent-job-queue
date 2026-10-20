package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/caddyauth"
)

func fixtureAdmission(t *testing.T, limits AuthAdmissionSettings) *authAdmission {
	t.Helper()
	c := fixtureController(t)
	settings := testSettings()
	settings.PasswordHash = string((caddyauth.BcryptHash{}).FakeHash())
	settings.AuthAdmission = &limits
	app := fixtureGatewayHTTPConfig(t, c.p, &settings)
	raw := app.Servers["remote"].Routes[0].HandlersRaw
	if len(raw) != 3 {
		t.Fatal("unexpected gateway handler chain")
	}
	var admission authAdmission
	var module struct {
		Handler string `json:"handler"`
	}
	if err := json.Unmarshal(raw[1], &module); err != nil || module.Handler != "agent_queue_auth_admission" {
		t.Fatal("generated gateway does not route authentication through admission", err)
	}
	if err := json.Unmarshal(raw[1], &admission); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)
	if err := admission.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	return &admission
}

func admissionRequest(peer, password string) *http.Request {
	r := httptest.NewRequest("GET", "https://queue.local/", nil)
	r.RemoteAddr = peer
	r.SetBasicAuth(username, password)
	return r.WithContext(context.WithValue(r.Context(), caddy.ReplacerCtxKey, caddy.NewReplacer()))
}

func admissionStatus(err error) int {
	if err == nil {
		return 200
	}
	var handler caddyhttp.HandlerError
	if errors.As(err, &handler) {
		return handler.StatusCode
	}
	return 500
}

func TestAuthAdmissionRealCaddyCacheAndDistinctGuesses(t *testing.T) {
	limits := defaultAuthAdmission()
	limits.Burst = 16
	a := fixtureAdmission(t, limits)
	clock := time.Now()
	a.now = func() time.Time { return clock }
	provider := a.Authentication.Providers["http_basic"].(*caddyauth.HTTPBasicAuth)
	counter := &countingGatewayComparer{delegate: provider.Hash}
	provider.Hash = counter
	next := caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { return nil })
	// One real cost-14 comparison warms the unmodified upstream cache. Initial
	// assets and cached dashboard polling consume only that single comparison.
	var assets sync.WaitGroup
	start := make(chan struct{})
	for i := range limits.Burst {
		assets.Add(1)
		go func() {
			defer assets.Done()
			<-start
			r := admissionRequest("192.0.2.10:1234", "antitiming")
			r.URL.Path = fmt.Sprintf("/asset-%d.js", i)
			if err := a.ServeHTTP(httptest.NewRecorder(), r, next); err != nil {
				t.Error("cold initial asset burst rejected", err)
			}
		}()
	}
	close(start)
	assets.Wait()
	if counter.calls.Load() != 1 {
		t.Fatal("cached polls recomputed bcrypt", counter.calls.Load())
	}
	w := httptest.NewRecorder()
	if status := admissionStatus(a.ServeHTTP(w, admissionRequest("192.0.2.10:9876", "distinct-guess"), next)); status != 429 || counter.calls.Load() != 1 || w.Header().Get("Retry-After") == "" {
		t.Fatal("rate rejection reached bcrypt or omitted retry response", status, counter.calls.Load())
	}
	clock = clock.Add(time.Second)
	if err := a.ServeHTTP(httptest.NewRecorder(), admissionRequest("192.0.2.10:1234", "antitiming"), next); err != nil {
		t.Fatal("cached polling did not refill", err)
	}
}

type gatedAdmissionComparer struct {
	entered chan struct{}
	proceed chan struct{}
	calls   atomic.Int64
	active  atomic.Int64
	peak    atomic.Int64
}

func (g *gatedAdmissionComparer) Compare(_, _ []byte) (bool, error) {
	g.calls.Add(1)
	active := g.active.Add(1)
	defer g.active.Add(-1)
	for old := g.peak.Load(); active > old; old = g.peak.Load() {
		if g.peak.CompareAndSwap(old, active) {
			break
		}
	}
	g.entered <- struct{}{}
	<-g.proceed
	return false, nil
}

func TestAuthAdmissionBoundsDistinctConcurrentBcryptAndCancelsWaiters(t *testing.T) {
	limits := defaultAuthAdmission()
	limits.MaxConcurrent, limits.Burst = 1, 2
	a := fixtureAdmission(t, limits)
	gate := &gatedAdmissionComparer{entered: make(chan struct{}, 4), proceed: make(chan struct{})}
	a.Authentication.Providers["http_basic"].(*caddyauth.HTTPBasicAuth).Hash = gate
	next := caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		t.Error("invalid password reached upstream")
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	first := admissionRequest("192.0.2.1:1", "distinct-one")
	first = first.WithContext(context.WithValue(ctx, caddy.ReplacerCtxKey, caddy.NewReplacer()))
	firstDone := make(chan error, 1)
	go func() { firstDone <- a.ServeHTTP(httptest.NewRecorder(), first, next) }()
	<-gate.entered
	cancel()
	// Cancellation cannot interrupt bcrypt. Its slot remains owned until the
	// synchronous comparison actually returns, so another comparison cannot
	// start while an abandoned one still burns CPU.
	if len(a.slots) != 1 {
		t.Fatal("active bcrypt lost its slot on cancellation")
	}
	var wait sync.WaitGroup
	cancels := make([]context.CancelFunc, limits.Burst)
	for i := range limits.Burst {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		r := admissionRequest(fmt.Sprintf("192.0.2.%d:1", i+2), fmt.Sprintf("distinct-%d", i+2))
		r = r.WithContext(context.WithValue(ctx, caddy.ReplacerCtxKey, caddy.NewReplacer()))
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := a.ServeHTTP(httptest.NewRecorder(), r, next); !errors.Is(err, context.Canceled) {
				t.Error("queued cancellation lost", err)
			}
		}()
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(a.waiting) != limits.Burst && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(a.waiting) != limits.Burst {
		t.Fatal("waiters did not enter bounded queue")
	}
	if status := admissionStatus(a.ServeHTTP(httptest.NewRecorder(), admissionRequest("192.0.2.100:1", "excess-guess"), next)); status != 429 {
		t.Fatal("queue overflow was admitted", status)
	}
	for _, cancel := range cancels {
		cancel()
	}
	wait.Wait()
	if len(a.waiting) != 0 || gate.calls.Load() != 1 {
		t.Fatal("cancelled waiters leaked or reached bcrypt")
	}
	close(gate.proceed)
	if status := admissionStatus(<-firstDone); status != 401 {
		t.Fatal(status)
	}
	if len(a.slots) != 0 || gate.peak.Load() != 1 {
		t.Fatal("authentication slot leak or concurrency breach")
	}
	if status := admissionStatus(a.ServeHTTP(httptest.NewRecorder(), admissionRequest("192.0.2.101:1", "after-release"), next)); status != 401 || gate.calls.Load() != 2 {
		t.Fatal("slot not reusable after failed authentication", status)
	}
}

func TestAuthAdmissionPeerBoundExpiryAndSocketIdentity(t *testing.T) {
	limits := defaultAuthAdmission()
	limits.Burst, limits.RequestsPerSecond, limits.MaxPeers, limits.PeerTTLSeconds = 1, 1, 2, 2
	a := fixtureAdmission(t, limits)
	clock := time.Now()
	a.now = func() time.Time { return clock }
	peer := netip.MustParseAddr("192.0.2.1")
	if !a.allowPeer(peer) || a.allowPeer(peer) {
		t.Fatal("peer burst not enforced")
	}
	clock = clock.Add(time.Second)
	if !a.allowPeer(peer) {
		t.Fatal("peer refill failed")
	}
	if !a.allowPeer(netip.MustParseAddr("192.0.2.2")) {
		t.Fatal("second peer failed")
	}
	for i := 3; i < 250; i++ {
		if a.allowPeer(netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", i))) {
			t.Fatal("full live table evicted a limiter")
		}
	}
	if len(a.peers) != 2 {
		t.Fatal("peer state grew past bound")
	}
	clock = clock.Add(2 * time.Second)
	if !a.allowPeer(netip.MustParseAddr("192.0.2.3")) || len(a.peers) != 1 {
		t.Fatal("idle peer state did not expire")
	}
	next := caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		t.Error("unauthenticated request reached API")
		return nil
	})
	r := admissionRequest("[::ffff:192.0.2.10]:1234", "")
	r.Header.Del("Authorization")
	r.Header.Set("Forwarded", "for=192.0.2.200")
	r.Header.Set("X-Forwarded-For", "192.0.2.201")
	if status := admissionStatus(a.ServeHTTP(httptest.NewRecorder(), r, next)); status != 401 {
		t.Fatal(status)
	}
	r = admissionRequest("192.0.2.10:9876", "")
	r.Header.Set("X-Forwarded-For", "192.0.2.202")
	if status := admissionStatus(a.ServeHTTP(httptest.NewRecorder(), r, next)); status != 429 {
		t.Fatal("port/header/IPv4 mapping bypassed peer rate", status)
	}
	r.RemoteAddr = "spoofed-invalid-peer"
	if status := admissionStatus(a.ServeHTTP(httptest.NewRecorder(), r, next)); status != 400 {
		t.Fatal("invalid socket peer did not fail closed", status)
	}
}

func TestAuthAdmissionReleasesBeforeProxyAndOnPanic(t *testing.T) {
	a := fixtureAdmission(t, defaultAuthAdmission())
	// Inject a provider at Caddy's documented interface to exercise boundary
	// ownership without another cost-14 comparison.
	a.Authentication.Providers = map[string]caddyauth.Authenticator{"fixture": admissionAuthenticator(func(http.ResponseWriter, *http.Request) (caddyauth.User, bool, error) {
		return caddyauth.User{ID: username}, true, nil
	})}
	if err := a.ServeHTTP(httptest.NewRecorder(), admissionRequest("192.0.2.1:1", "fixture"), caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		if len(a.slots) != 0 {
			t.Error("API work held authentication slot")
		}
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	a.Authentication.Providers["fixture"] = admissionAuthenticator(func(http.ResponseWriter, *http.Request) (caddyauth.User, bool, error) { panic("fixture") })
	func() {
		defer func() {
			if recover() == nil {
				t.Error("fixture did not panic")
			}
		}()
		_ = a.ServeHTTP(httptest.NewRecorder(), admissionRequest("192.0.2.1:1", "fixture"), nil)
	}()
	if len(a.slots) != 0 {
		t.Fatal("panic leaked authentication slot")
	}
}

type admissionAuthenticator func(http.ResponseWriter, *http.Request) (caddyauth.User, bool, error)

func (f admissionAuthenticator) Authenticate(w http.ResponseWriter, r *http.Request) (caddyauth.User, bool, error) {
	return f(w, r)
}

func TestAuthAdmissionConfigurationAndRotation(t *testing.T) {
	limits := defaultAuthAdmission()
	encoded, _ := json.Marshal(limits)
	if _, err := parseAuthAdmission(encoded); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"null", "{}", `{"max_concurrent":1,"max_concurrent":2}`, `{"max_concurrent":1,"requests_per_second":1,"burst":64,"max_peers":1,"peer_ttl_seconds":1}`} {
		if _, err := parseAuthAdmission(json.RawMessage(raw)); err == nil {
			t.Fatal("invalid admission accepted", raw)
		}
	}
	for _, field := range []string{"concurrency", "rate", "burst", "peers", "ttl"} {
		bad := limits
		switch field {
		case "concurrency":
			bad.MaxConcurrent = 0
		case "rate":
			bad.RequestsPerSecond = 0
		case "burst":
			bad.Burst = 0
		case "peers":
			bad.MaxPeers = 0
		case "ttl":
			bad.PeerTTLSeconds = 1
		}
		if err := bad.validate(); err == nil {
			t.Fatal("invalid limit accepted", field)
		}
	}
	c := fixtureController(t)
	settings := testSettings()
	limits.MaxConcurrent = 3
	settings.AuthAdmission = &limits
	if err := writePrivate(c.p.config, settings); err != nil {
		t.Fatal(err)
	}
	// Apply the already prepared credential just as configure does after its
	// password hashing phase. Rotation must retain operator admission settings.
	if err := c.applyConfiguration(&preparedConfiguration{passwordHash: settings.PasswordHash, port: settings.Port}); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSettings(c.p)
	if err != nil || loaded.AuthAdmission == nil || *loaded.AuthAdmission != limits {
		t.Fatal("rotation reset admission limits", err)
	}
	status, err := c.status()
	if err != nil || status["authAdmission"] != limits {
		t.Fatal("effective admission limits absent from status", err)
	}
	body := fmt.Sprintf(`{"password":"fixture","auth_admission":%s}`, encoded)
	if _, err := readConfigurationInput(strings.NewReader(body)); err != nil {
		t.Fatal("configure cannot accept documented settings", err)
	}
}

type shortCacheComparer struct {
	calls      atomic.Int64
	sharedTail atomic.Bool
}

func (c *shortCacheComparer) Compare(hash, password []byte) (bool, error) {
	c.calls.Add(1)
	if cap(hash) != len(hash) {
		c.sharedTail.Store(true)
	}
	return string(password) == "0124", nil
}

func TestGatewayConcurrentShortCredentialsHaveIndependentCaddyCacheKeys(t *testing.T) {
	c := fixtureController(t)
	settings := testSettings()
	settings.PasswordHash = string((caddyauth.BcryptHash{}).FakeHash())
	provider := fixtureGatewayBasicAuth(t, c.p, &settings)
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	if err := provider.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	counter := new(shortCacheComparer)
	provider.Hash = counter
	// Exercise the real upstream cache twice, including unknown users and all
	// four-character guesses. The fixture comparer avoids 500 bcrypt calls;
	// the separate real-bcrypt fixture proves credential semantics and cost.
	for round := range 2 {
		var wait sync.WaitGroup
		start := make(chan struct{})
		for i := range 500 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				password := fmt.Sprintf("%04d", i)
				account := username
				if i%2 != 0 {
					account = "missing-fixture-user"
				}
				r := httptest.NewRequest("GET", "https://queue.local/", nil)
				r.SetBasicAuth(account, password)
				_, accepted, err := provider.Authenticate(httptest.NewRecorder(), r)
				if err != nil || accepted != (account == username && password == "0124") {
					t.Error("upstream cache confused distinct credentials", round, i, accepted, err)
				}
			}()
		}
		close(start)
		wait.Wait()
	}
	if counter.calls.Load() != 500 {
		t.Fatal("upstream cache no longer reused short credential results", counter.calls.Load())
	}
	if counter.sharedTail.Load() {
		t.Fatal("Caddy hash backing slice permits shared cache-key append writes")
	}
}

func TestGatewayRealBcryptShortCredentialPreservesSettingsAndCache(t *testing.T) {
	c := fixtureController(t)
	settings := testSettings()
	// Public test-only credential "1234", generated at the production cost14.
	// It is never installed as a default or used outside temporary fixtures.
	settings.PasswordHash = "$2a$14$6NXuPGlS/ACMWZYdSt8eH.ncdh8YrRchpIx7pawx9aEo/7qFa/ARe"
	if err := writePrivate(c.p.config, settings); err != nil {
		t.Fatal(err)
	}
	provider := fixtureGatewayBasicAuth(t, c.p, &settings)
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	if err := provider.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	counter := &countingGatewayComparer{delegate: provider.Hash}
	provider.Hash = counter
	for _, password := range []string{"1234", "1234", "4321", "4321"} {
		r := httptest.NewRequest("GET", "https://queue.local/", nil)
		r.SetBasicAuth(username, password)
		user, accepted, err := provider.Authenticate(httptest.NewRecorder(), r)
		if err != nil || accepted != (password == "1234") || accepted && user.ID != username {
			t.Fatal("supported hash encoding changed real bcrypt authentication", accepted, err)
		}
	}
	if counter.calls.Load() != 2 {
		t.Fatal("short credentials bypassed Caddy cache", counter.calls.Load())
	}
	loaded, err := loadSettings(c.p)
	if err != nil || loaded.PasswordHash != settings.PasswordHash {
		t.Fatal("gateway hash encoding changed durable bcrypt settings", err)
	}
}
