package remote

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/caddyauth"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/headers"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	"github.com/caddyserver/caddy/v2/modules/caddypki"
	"github.com/caddyserver/caddy/v2/modules/caddytls"
	"github.com/caddyserver/caddy/v2/modules/filestorage"
	"github.com/caddyserver/caddy/v2/modules/logging"
	"golang.org/x/crypto/bcrypt"
)

const embeddedGateway = "embedded-caddy-v1"

func prepareGatewayLog(p remotePaths) error {
	path := filepath.Join(p.state, "gateway.log")
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_WRONLY|syscall.O_APPEND|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return errors.New("Private gateway log could not be opened safely")
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !ownedByCurrentUser(info) || info.Mode().Perm()&0077 != 0 {
		return errors.New("Unsafe private gateway log")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return errors.New("Unsafe private gateway log links")
	}
	return nil
}

type gatewayJSON struct{ err error }

func (b *gatewayJSON) raw(value any) json.RawMessage {
	if b.err != nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		b.err = err
	}
	return data
}
func (b *gatewayJSON) module(value any, key, name string) json.RawMessage {
	data := b.raw(value)
	if b.err != nil {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		b.err = err
		return nil
	}
	fields[key] = b.raw(name)
	return b.raw(fields)
}

// gatewayConfiguration constructs Caddy JSON from validated settings. It does
// not provision modules, create PKI material, mutate global state, or bind ports.
func gatewayConfiguration(p remotePaths, settings *Settings) ([]byte, error) {
	if settings == nil || len(settings.Hosts) == 0 || settings.Port < 1024 || settings.Port > 65535 || settings.Username != username || !bcryptHash.MatchString(settings.PasswordHash) {
		return nil, errors.New("Invalid embedded gateway settings")
	}
	for _, host := range settings.Hosts {
		if !hostPattern.MatchString(host) {
			return nil, errors.New("Invalid remote host configuration")
		}
	}
	if cost, err := bcrypt.Cost([]byte(settings.PasswordHash)); err != nil || cost != 14 {
		return nil, errors.New("Invalid remote credential configuration")
	}
	b := new(gatewayJSON)
	disabled := false
	// Match Caddy's basic_auth adapter default: its bounded upstream cache and
	// singleflight keep each dashboard poll from recomputing cost-14 bcrypt.
	basic := caddyauth.HTTPBasicAuth{HashRaw: b.raw(map[string]string{"algorithm": "bcrypt"}), AccountList: []caddyauth.Account{{Username: username, Password: settings.PasswordHash}}, HashCache: &caddyauth.Cache{}}
	auth := caddyauth.Authentication{ProvidersRaw: caddy.ModuleMap{"http_basic": b.raw(basic)}}
	responseHeaders := headers.Handler{Response: &headers.RespHeaderOps{HeaderOps: &headers.HeaderOps{Delete: []string{"Server"}}, Deferred: true}}
	proxy := reverseproxy.Handler{Upstreams: reverseproxy.UpstreamPool{&reverseproxy.Upstream{Dial: "unix/" + p.socket}}, Headers: &headers.Handler{Request: &headers.HeaderOps{Delete: []string{"Authorization", "Forwarded", "X-Forwarded-Host"}, Set: http.Header{"X-Forwarded-Proto": []string{"https"}}}}}
	httpApp := caddyhttp.App{GracePeriod: caddy.Duration(time.Second), Servers: map[string]*caddyhttp.Server{"remote": {
		Listen: []string{net.JoinHostPort(settings.Hosts[0], fmt.Sprint(settings.Port))}, Protocols: []string{"h1", "h2"}, AutoHTTPS: &caddyhttp.AutoHTTPSConfig{DisableRedir: true}, TLSConnPolicies: caddytls.ConnectionPolicies{&caddytls.ConnectionPolicy{ProtocolMin: "tls1.2"}},
		ReadHeaderTimeout: caddy.Duration(5 * time.Second), ReadTimeout: caddy.Duration(30 * time.Second), WriteTimeout: caddy.Duration(30 * time.Second), IdleTimeout: caddy.Duration(30 * time.Second), MaxHeaderBytes: 16384,
		Routes: caddyhttp.RouteList{{MatcherSetsRaw: caddyhttp.RawMatcherSets{caddy.ModuleMap{"host": b.raw(settings.Hosts)}}, HandlersRaw: []json.RawMessage{b.module(responseHeaders, "handler", "headers"), b.module(auth, "handler", "authentication"), b.module(proxy, "handler", "reverse_proxy")}, Terminal: true}},
	}}}
	tlsApp := caddytls.TLS{Automation: &caddytls.AutomationConfig{Policies: []*caddytls.AutomationPolicy{{SubjectsRaw: settings.Hosts, IssuersRaw: []json.RawMessage{b.module(caddytls.InternalIssuer{CA: "local"}, "module", "internal")}}}}}
	pkiApp := caddypki.PKI{CAs: map[string]*caddypki.CA{"local": {InstallTrust: &disabled}}}
	writer := logging.FileWriter{Filename: filepath.Join(p.state, "gateway.log"), RollSizeMB: 2, RollKeep: 3, RollKeepDays: 7, RollCompression: "none"}
	config := caddy.Config{Admin: &caddy.AdminConfig{Disabled: true, Config: &caddy.ConfigSettings{Persist: &disabled}}, StorageRaw: b.module(filestorage.FileStorage{Root: filepath.Join(p.state, "caddy")}, "module", "file_system"),
		Logging: &caddy.Logging{Logs: map[string]*caddy.CustomLog{"default": {BaseLog: caddy.BaseLog{WriterRaw: b.module(writer, "output", "file"), Level: "WARN"}}}},
		AppsRaw: caddy.ModuleMap{"http": b.raw(httpApp), "tls": b.raw(tlsApp), "pki": b.raw(pkiApp)}}
	if b.err != nil {
		return nil, b.err
	}
	data, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	var checked caddy.Config
	if err := caddy.StrictUnmarshalJSON(data, &checked); err != nil {
		return nil, err
	}
	return data, nil
}

func readCAAsset(path string, private bool) ([]byte, bool, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errors.New("Existing remote certificate material could not be verified")
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	var links uint64
	if err == nil {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			links = uint64(stat.Nlink)
		}
	}
	if err != nil || !info.Mode().IsRegular() || !ownedByCurrentUser(info) || links != 1 || info.Mode().Perm()&0022 != 0 || private && info.Mode().Perm()&0077 != 0 || info.Size() > 131072 {
		return nil, false, errors.New("Unsafe existing remote certificate material")
	}
	body, err := io.ReadAll(io.LimitReader(file, 131073))
	if err != nil || len(body) > 131072 {
		return nil, false, errors.New("Existing remote certificate material could not be read")
	}
	return body, true, nil
}

// Caddy can regenerate a missing root even when old keys remain. Refuse partial
// or mismatched CA material before Load so upgrades never silently replace phone
// trust. A complete root pair without an intermediate can safely mint one under
// that same root.
func validateCAStorage(p remotePaths) error {
	directory := filepath.Dir(p.certificate)
	for current := directory; current != p.state && current != filepath.Dir(current); current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.IsDir() || !ownedByCurrentUser(info) || info.Mode().Perm()&0022 != 0 {
			return errors.New("Unsafe existing remote certificate directory")
		}
	}
	rootCert, hasRootCert, err := readCAAsset(filepath.Join(directory, "root.crt"), false)
	if err != nil {
		return err
	}
	rootKey, hasRootKey, err := readCAAsset(filepath.Join(directory, "root.key"), true)
	if err != nil {
		return err
	}
	interCert, hasInterCert, err := readCAAsset(filepath.Join(directory, "intermediate.crt"), false)
	if err != nil {
		return err
	}
	interKey, hasInterKey, err := readCAAsset(filepath.Join(directory, "intermediate.key"), true)
	if err != nil {
		return err
	}
	if hasRootCert != hasRootKey || hasInterCert != hasInterKey || !hasRootCert && (hasInterCert || hasInterKey) {
		return errors.New("Remote certificate authority storage is incomplete; restore its matching existing certificates and keys before enabling access")
	}
	if !hasRootCert {
		return nil
	}
	rootPair, err := tls.X509KeyPair(rootCert, rootKey)
	if err != nil || len(rootPair.Certificate) != 1 {
		return errors.New("Remote certificate authority root certificate and key do not match")
	}
	root, err := x509.ParseCertificate(rootPair.Certificate[0])
	if err != nil || !root.IsCA || !root.BasicConstraintsValid {
		return errors.New("Remote certificate authority root is invalid")
	}
	if hasInterCert {
		pair, err := tls.X509KeyPair(interCert, interKey)
		if err != nil || len(pair.Certificate) != 1 {
			return errors.New("Remote certificate authority intermediate certificate and key do not match")
		}
		intermediate, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil || !intermediate.IsCA || intermediate.CheckSignatureFrom(root) != nil {
			return errors.New("Remote certificate authority intermediate does not match its trusted root")
		}
	}
	return nil
}

// gatewayReady validates the actual private API and root-trusted HTTPS Basic
// challenge. Stored bcrypt hashes cannot reconstruct a password; authenticated
// mutations are checked by the isolated real-gateway acceptance suite.
func gatewayReady(ctx context.Context, p remotePaths, settings *Settings) bool {
	certificate, exists, err := readCAAsset(p.certificate, false)
	if err != nil || !exists {
		return false
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificate) {
		return false
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("https://%s:%d/api/csrf", settings.Hosts[0], settings.Port), nil)
	if err != nil {
		return false
	}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(response.Header.Get("Www-Authenticate"), "Basic ") {
		return false
	}
	unixTransport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", p.socket)
	}, DisableKeepAlives: true}
	defer unixTransport.CloseIdleConnections()
	request, err = http.NewRequestWithContext(ctx, "GET", "http://private/api/csrf", nil)
	if err != nil {
		return false
	}
	request.Host = fmt.Sprintf("%s:%d", settings.Hosts[0], settings.Port)
	request.Header.Set("X-Forwarded-Proto", "https")
	response, err = (&http.Client{Transport: unixTransport, Timeout: time.Second}).Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	var token struct {
		Token string `json:"token"`
	}
	return response.StatusCode == 200 && json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&token) == nil && token.Token != ""
}
