package remote

import (
	"bytes"
	"container/list"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/caddyauth"
	"golang.org/x/time/rate"
)

// AuthAdmissionSettings controls work admitted to Caddy authentication. All
// requests count, including cached credentials; no password cache is added here.
type AuthAdmissionSettings struct {
	MaxConcurrent     int     `json:"max_concurrent"`
	RequestsPerSecond float64 `json:"requests_per_second"`
	Burst             int     `json:"burst"`
	MaxPeers          int     `json:"max_peers"`
	PeerTTLSeconds    int     `json:"peer_ttl_seconds"`
}

func defaultAuthAdmission() AuthAdmissionSettings {
	return AuthAdmissionSettings{MaxConcurrent: 2, RequestsPerSecond: 4, Burst: 64, MaxPeers: 1024, PeerTTLSeconds: 600}
}

func (s AuthAdmissionSettings) validate() error {
	if s.MaxConcurrent < 1 || s.MaxConcurrent > 32 || math.IsNaN(s.RequestsPerSecond) || math.IsInf(s.RequestsPerSecond, 0) || s.RequestsPerSecond < 0.1 || s.RequestsPerSecond > 100 || s.Burst < 1 || s.Burst > 512 || s.MaxPeers < 1 || s.MaxPeers > 65536 || s.PeerTTLSeconds < 1 || s.PeerTTLSeconds > 86400 || float64(s.PeerTTLSeconds) < float64(s.Burst)/s.RequestsPerSecond {
		return errors.New("Invalid auth_admission: max_concurrent 1-32, requests_per_second 0.1-100, burst 1-512, max_peers 1-65536, peer_ttl_seconds 1-86400 and at least burst / requests_per_second")
	}
	return nil
}

func parseAuthAdmission(raw json.RawMessage) (*AuthAdmissionSettings, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	fields, err := decodeObject(raw)
	if err != nil || !keysAre(fields, "max_concurrent", "requests_per_second", "burst", "max_peers", "peer_ttl_seconds") {
		return nil, errors.New("auth_admission requires all five documented limits")
	}
	var settings AuthAdmissionSettings
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return nil, errors.New("Invalid auth_admission limits")
	}
	if err := settings.validate(); err != nil {
		return nil, err
	}
	return &settings, nil
}

func init() { caddy.RegisterModule(&authAdmission{}) }

// authAdmission delegates authentication entirely to Caddy. Keeping the
// authentication boundary here lets us release a slot before proxying any API
// work, without weakening Caddy's bcrypt, cache, singleflight, or challenges.
type authAdmission struct {
	Limits         AuthAdmissionSettings    `json:"limits"`
	Authentication caddyauth.Authentication `json:"authentication"`

	slots   chan struct{}
	waiting chan struct{}
	mu      sync.Mutex
	peers   map[netip.Addr]*list.Element
	idle    list.List
	now     func() time.Time
}

type admissionPeer struct {
	address netip.Addr
	limiter *rate.Limiter
	seen    time.Time
}

func (*authAdmission) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "http.handlers.agent_queue_auth_admission", New: func() caddy.Module { return new(authAdmission) }}
}

func (a *authAdmission) Provision(ctx caddy.Context) error {
	if err := a.Limits.validate(); err != nil {
		return err
	}
	a.slots = make(chan struct{}, a.Limits.MaxConcurrent)
	a.waiting = make(chan struct{}, a.Limits.Burst)
	a.peers = make(map[netip.Addr]*list.Element)
	a.now = time.Now
	return a.Authentication.Provision(ctx)
}

// allowPeer expires idle entries in last-seen order. A full table rejects new
// peers instead of evicting a live limiter and granting its next request a fresh
// burst. TTL is at least a full refill, so expiry cannot reset limits early.
func (a *authAdmission) allowPeer(address netip.Addr) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for head := a.idle.Front(); head != nil; head = a.idle.Front() {
		peer := head.Value.(*admissionPeer)
		if now.Sub(peer.seen) < time.Duration(a.Limits.PeerTTLSeconds)*time.Second {
			break
		}
		delete(a.peers, peer.address)
		a.idle.Remove(head)
	}
	element := a.peers[address]
	if element == nil {
		if len(a.peers) >= a.Limits.MaxPeers {
			return false
		}
		peer := &admissionPeer{address: address, limiter: rate.NewLimiter(rate.Limit(a.Limits.RequestsPerSecond), a.Limits.Burst)}
		element = a.idle.PushBack(peer)
		a.peers[address] = element
	}
	peer := element.Value.(*admissionPeer)
	peer.seen = now
	a.idle.MoveToBack(element)
	return peer.limiter.AllowN(now, 1)
}

func admissionRejected(w http.ResponseWriter) error {
	w.Header().Set("Retry-After", "1")
	w.Header().Set("Cache-Control", "no-store")
	return caddyhttp.Error(http.StatusTooManyRequests, errors.New("Remote authentication admission limit reached"))
}

func (a *authAdmission) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if err := r.Context().Err(); err != nil {
		return err
	}
	// RemoteAddr is the socket peer supplied by net/http. Forwarded headers and
	// Caddy's client-IP placeholders are deliberately not an authority here.
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return caddyhttp.Error(http.StatusBadRequest, errors.New("Invalid remote socket peer"))
	}
	if !a.allowPeer(peer.Addr().Unmap()) {
		return admissionRejected(w)
	}
	select {
	case a.slots <- struct{}{}:
	default:
		// A bounded queue accommodates a cold browser asset burst while Caddy
		// coalesces its first password verification. No unbounded waiters or
		// background workers survive the request.
		select {
		case a.waiting <- struct{}{}:
		default:
			return admissionRejected(w)
		}
		timer := time.NewTimer(gatewayResponseTimeout)
		defer timer.Stop()
		select {
		case a.slots <- struct{}{}:
			<-a.waiting
		case <-r.Context().Done():
			<-a.waiting
			return r.Context().Err()
		case <-timer.C:
			<-a.waiting
			return admissionRejected(w)
		}
	}
	var once sync.Once
	release := func() { once.Do(func() { <-a.slots }) }
	defer release()
	if err := r.Context().Err(); err != nil {
		return err
	}
	return a.Authentication.ServeHTTP(w, r, caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		release()
		if err := r.Context().Err(); err != nil {
			return err
		}
		return next.ServeHTTP(w, r)
	}))
}

var (
	_ caddy.Provisioner           = (*authAdmission)(nil)
	_ caddyhttp.MiddlewareHandler = (*authAdmission)(nil)
)
