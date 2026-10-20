package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/caddyauth"
)

func fixtureGatewayHTTPConfig(t *testing.T, p remotePaths, settings *Settings) caddyhttp.App {
	t.Helper()
	body, err := gatewayConfiguration(p, settings)
	if err != nil {
		t.Fatal(err)
	}
	var config caddy.Config
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatal(err)
	}
	var httpApp caddyhttp.App
	if err := json.Unmarshal(config.AppsRaw["http"], &httpApp); err != nil {
		t.Fatal(err)
	}
	return httpApp
}

func fixtureGatewayBasicAuth(t *testing.T, p remotePaths, settings *Settings) caddyauth.HTTPBasicAuth {
	t.Helper()
	httpApp := fixtureGatewayHTTPConfig(t, p, settings)
	server := httpApp.Servers["remote"]
	if server == nil || len(server.Routes) != 1 {
		t.Fatal("fixture could not locate generated gateway route")
	}
	for _, raw := range server.Routes[0].HandlersRaw {
		var handler struct {
			Handler string `json:"handler"`
		}
		if err := json.Unmarshal(raw, &handler); err != nil {
			t.Fatal(err)
		}
		if handler.Handler == "agent_queue_auth_admission" {
			var admission authAdmission
			if err := json.Unmarshal(raw, &admission); err != nil {
				t.Fatal(err)
			}
			var basic caddyauth.HTTPBasicAuth
			if err := json.Unmarshal(admission.Authentication.ProvidersRaw["http_basic"], &basic); err != nil {
				t.Fatal(err)
			}
			return basic
		}
	}
	t.Fatal("fixture could not locate generated gateway Basic auth")
	return caddyauth.HTTPBasicAuth{}
}

func fixtureGatewayRequestBudget(t *testing.T, p remotePaths, settings *Settings) time.Duration {
	t.Helper()
	server := fixtureGatewayHTTPConfig(t, p, settings).Servers["remote"]
	if server == nil || server.WriteTimeout <= 0 {
		t.Fatal("gateway must define a bounded response deadline")
	}
	// Exercise the service's actual response contract. Race instrumentation makes
	// the unchanged cost-14 bcrypt operation about twelve times slower, so a
	// separate five-second fixture limit abandoned valid first authentications.
	return time.Duration(server.WriteTimeout)
}

type countingGatewayComparer struct {
	delegate caddyauth.Comparer
	calls    atomic.Int64
}

func (c *countingGatewayComparer) Compare(hash, password []byte) (bool, error) {
	c.calls.Add(1)
	return c.delegate.Compare(hash, password)
}

func TestGatewayRepeatedAuthenticationUsesCaddyBoundedCache(t *testing.T) {
	c := fixtureController(t)
	settings := testSettings()
	// Caddy publishes this public anti-timing fixture hash and its password. It
	// retains production bcrypt cost14 without spending another hash generation.
	settings.PasswordHash = string((caddyauth.BcryptHash{}).FakeHash())
	provider := fixtureGatewayBasicAuth(t, c.p, &settings)
	if provider.HashCache == nil {
		t.Fatal("generated Basic auth omitted Caddy's bounded cache")
	}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	if err := provider.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	counter := &countingGatewayComparer{delegate: provider.Hash}
	provider.Hash = counter
	for _, password := range []string{"antitiming", "antitiming", "wrong-fixture", "wrong-fixture"} {
		request := httptest.NewRequest("GET", "https://queue.local/", nil)
		request.SetBasicAuth(username, password)
		user, accepted, err := provider.Authenticate(httptest.NewRecorder(), request)
		if err != nil || accepted != (password == "antitiming") || accepted && user.ID != username {
			t.Fatal("real Caddy bcrypt authentication changed", accepted, err)
		}
	}
	if counter.calls.Load() != 2 {
		t.Fatal("repeated authenticated dashboard polls recomputed bcrypt", counter.calls.Load())
	}
}

func assertLegacyBasicAuthParity(t *testing.T, p remotePaths, settings *Settings, body []byte) {
	t.Helper()
	var config any
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatal(err)
	}
	var providers []caddyauth.HTTPBasicAuth
	var visit func(any)
	visit = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			for key, child := range node {
				if key == "http_basic" {
					raw, err := json.Marshal(child)
					if err != nil {
						t.Fatal(err)
					}
					var basic caddyauth.HTTPBasicAuth
					if err := json.Unmarshal(raw, &basic); err != nil {
						t.Fatal(err)
					}
					providers = append(providers, basic)
				}
				visit(child)
			}
		case []any:
			for _, child := range node {
				visit(child)
			}
		}
	}
	visit(config)
	if len(providers) != 1 || providers[0].HashCache == nil {
		t.Fatal("legacy Caddy adapter did not retain its default bounded auth cache")
	}
	legacy, err := json.Marshal(providers[0])
	if err != nil {
		t.Fatal(err)
	}
	embeddedProvider := fixtureGatewayBasicAuth(t, p, settings)
	// Compare credential semantics across the supported upstream encodings.
	// Embedded provisioning uses base64 to avoid Caddy's short-input cache race.
	for i := range embeddedProvider.AccountList {
		hash, err := base64.StdEncoding.DecodeString(embeddedProvider.AccountList[i].Password)
		if err != nil {
			t.Fatal(err)
		}
		embeddedProvider.AccountList[i].Password = string(hash)
	}
	embedded, err := json.Marshal(embeddedProvider)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(legacy, embedded) {
		t.Fatal("embedded Basic auth differs from the reviewed legacy Caddy adapter")
	}
}

func TestGatewayConfigurationIsPureAndRestrictsSelectedModules(t *testing.T) {
	c := fixtureController(t)
	settings := testSettings()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	settings.Hosts = []string{"127.0.0.1", "queue.local"}
	settings.Port = listener.Addr().(*net.TCPAddr).Port
	beforeStorage := caddy.DefaultStorage
	body, err := gatewayConfiguration(c.p, &settings)
	if err != nil {
		t.Fatal(err)
	}
	if caddy.DefaultStorage != beforeStorage {
		t.Fatal("pure configuration mutated global Caddy storage")
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatal(err)
	}
	for _, setting := range []string{`"disabled":true`, `"persist":false`, `"install_trust":false`, `"hash_cache":{}`, `"protocols":["h1","h2"]`, `"disable_redirects":true`, `"grace_period":1000000000`, `"roll_size_mb":2`, `"roll_keep":3`, `"Authorization"`, `"Forwarded"`, `"X-Forwarded-Host"`, `"X-Forwarded-Proto":["https"]`} {
		if !bytes.Contains(body, []byte(setting)) {
			t.Fatal("missing gateway boundary", setting)
		}
	}
	if !bytes.Contains(body, []byte(filepath.Join(c.p.state, "caddy"))) || !bytes.Contains(body, []byte("unix/"+c.p.socket)) {
		t.Fatal("gateway storage or private upstream changed")
	}
	for _, path := range []string{c.p.certificate, filepath.Join(c.p.state, "gateway.log"), c.p.socket} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("configuration provisioned service state", path, err)
		}
	}
	for _, module := range []string{"http", "tls", "pki", "caddy.storage.file_system", "caddy.logging.writers.file", "http.handlers.authentication", "http.authentication.providers.http_basic", "http.authentication.hashes.bcrypt", "http.handlers.headers", "http.handlers.reverse_proxy", "tls.issuance.internal"} {
		if _, err := caddy.GetModule(module); err != nil {
			t.Fatal("required selected Caddy module missing", module, err)
		}
	}
}

func TestRemoteConfigureDoesNotProvisionPKIOrAutosaveGlobalConfiguration(t *testing.T) {
	c := fixtureController(t)
	originalAutosave := caddy.ConfigAutosavePath
	caddy.ConfigAutosavePath = filepath.Join(c.p.state, "forbidden-autosave.json")
	t.Cleanup(func() { caddy.ConfigAutosavePath = originalAutosave })
	beforeStorage := caddy.DefaultStorage
	value, err := decodeObject([]byte(`{"password":"fixture","port":8743}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.configure(value); err != nil {
		t.Fatal(err)
	}
	if caddy.DefaultStorage != beforeStorage {
		t.Fatal("configure mutated global Caddy storage")
	}
	for _, path := range []string{c.p.certificate, c.p.socket, c.p.gatewayConfig, caddy.ConfigAutosavePath, filepath.Join(c.p.state, "gateway.log")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("configure provisioned gateway state", path, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(c.p.state, "caddy"))
	if err != nil || len(entries) != 0 {
		t.Fatal("configure generated certificate material", err)
	}
}

func makeCAAssets(t *testing.T, p remotePaths) map[string][]byte {
	t.Helper()
	rootPublic, rootPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	interPublic, interPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Private fixture root"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	intermediate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Private fixture intermediate"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(12 * time.Hour), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, rootPublic, rootPrivate)
	if err != nil {
		t.Fatal(err)
	}
	interDER, err := x509.CreateCertificate(rand.Reader, intermediate, root, interPublic, rootPrivate)
	if err != nil {
		t.Fatal(err)
	}
	rootKey, err := x509.MarshalPKCS8PrivateKey(rootPrivate)
	if err != nil {
		t.Fatal(err)
	}
	interKey, err := x509.MarshalPKCS8PrivateKey(interPrivate)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"root.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}), "root.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rootKey}), "intermediate.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: interDER}), "intermediate.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: interKey})}
	directory := filepath.Dir(p.certificate)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(directory, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func TestGatewayRefusesIncompleteOrUnsafeCAWithoutReplacingTrust(t *testing.T) {
	for _, scenario := range []string{"missing-root-cert", "missing-root-key", "missing-intermediate-key", "missing-root-pair", "mismatched-root-key", "mismatched-intermediate-key", "public-key", "writable-cert", "symlink-cert", "hardlinked-key"} {
		t.Run(scenario, func(t *testing.T) {
			c := fixtureController(t)
			files := makeCAAssets(t, c.p)
			directory := filepath.Dir(c.p.certificate)
			switch scenario {
			case "missing-root-cert":
				_ = os.Remove(filepath.Join(directory, "root.crt"))
				delete(files, "root.crt")
			case "missing-root-key":
				_ = os.Remove(filepath.Join(directory, "root.key"))
				delete(files, "root.key")
			case "missing-intermediate-key":
				_ = os.Remove(filepath.Join(directory, "intermediate.key"))
				delete(files, "intermediate.key")
			case "missing-root-pair":
				for _, name := range []string{"root.crt", "root.key"} {
					_ = os.Remove(filepath.Join(directory, name))
					delete(files, name)
				}
			case "mismatched-root-key":
				files["root.key"] = files["intermediate.key"]
				_ = os.WriteFile(filepath.Join(directory, "root.key"), files["root.key"], 0600)
			case "mismatched-intermediate-key":
				files["intermediate.key"] = files["root.key"]
				_ = os.WriteFile(filepath.Join(directory, "intermediate.key"), files["intermediate.key"], 0600)
			case "public-key":
				_ = os.Chmod(filepath.Join(directory, "root.key"), 0644)
			case "writable-cert":
				_ = os.Chmod(filepath.Join(directory, "root.crt"), 0622)
			case "symlink-cert":
				target := filepath.Join(c.p.state, "fixture-root.crt")
				_ = os.Rename(filepath.Join(directory, "root.crt"), target)
				_ = os.Symlink(target, filepath.Join(directory, "root.crt"))
			case "hardlinked-key":
				_ = os.Link(filepath.Join(directory, "root.key"), filepath.Join(c.p.state, "fixture-hardlink"))
			}
			if err := validateCAStorage(c.p); err == nil {
				t.Fatal("accepted unsafe/incomplete CA", scenario)
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != len(files) {
				t.Fatal("CA guard changed stored assets", err)
			}
			for name, want := range files {
				body, err := os.ReadFile(filepath.Join(directory, name))
				if err != nil || !bytes.Equal(body, want) {
					t.Fatal("CA guard replaced existing trust material", name, err)
				}
			}
			for _, path := range []string{c.p.socket, filepath.Join(c.p.state, "gateway.log")} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("CA guard opened service state", path)
				}
			}
		})
	}
}

func TestGatewayAcceptsFreshOrMatchingExistingCAWithoutGeneratingFiles(t *testing.T) {
	c := fixtureController(t)
	if err := validateCAStorage(c.p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(c.p.certificate)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty CA guard generated material")
	}
	files := makeCAAssets(t, c.p)
	if err := validateCAStorage(c.p); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"intermediate.crt", "intermediate.key"} {
		_ = os.Remove(filepath.Join(filepath.Dir(c.p.certificate), name))
	}
	if err := validateCAStorage(c.p); err != nil {
		t.Fatal("complete root pair cannot renew its intermediate", err)
	}
	body, _ := os.ReadFile(c.p.certificate)
	if !bytes.Equal(body, files["root.crt"]) {
		t.Fatal("existing trust root changed")
	}
}

func TestGatewayLogRefusesUnsafeExistingTargets(t *testing.T) {
	for _, scenario := range []string{"symlink", "hardlink", "public", "directory"} {
		t.Run(scenario, func(t *testing.T) {
			c := fixtureController(t)
			path := filepath.Join(c.p.state, "gateway.log")
			target := filepath.Join(c.p.state, "private-target")
			_ = os.WriteFile(target, []byte("private fixture"), 0600)
			switch scenario {
			case "symlink":
				_ = os.Symlink(target, path)
			case "hardlink":
				_ = os.Link(target, path)
			case "public":
				_ = os.WriteFile(path, nil, 0644)
				_ = os.Chmod(path, 0644)
			case "directory":
				_ = os.Mkdir(path, 0700)
			}
			if err := prepareGatewayLog(c.p); err == nil {
				t.Fatal("accepted unsafe gateway log")
			}
			body, _ := os.ReadFile(target)
			if string(body) != "private fixture" {
				t.Fatal("gateway log modified another private file")
			}
		})
	}
	c := fixtureController(t)
	if err := prepareGatewayLog(c.p); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(c.p.state, "gateway.log"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("gateway log creation is not private", err)
	}
}

func TestRemoteHelpIsAvailableWithoutSettingsOrDependencies(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("AGENT_REMOTE_TEST_MODE", "1")
	t.Setenv("AGENT_REMOTE_STATE", "invalid-relative-fixture-path")
	for _, flag := range []string{"--help", "-h"} {
		for _, action := range []string{"", "status", "configure", "enable", "disable", "serve"} {
			args := []string{flag}
			if action != "" {
				args = []string{action, flag}
			}
			if code := Main(args); code != 0 {
				t.Fatalf("help needs valid state or dependencies: %v: %d", args, code)
			}
		}
	}
}

func TestGatewayCannotStartThroughUnmanagedEntryPoints(t *testing.T) {
	c := fixtureController(t)
	settings := testSettings()
	settings.Enabled = true
	if err := writePrivate(c.p.config, settings); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_REMOTE_TEST_HELPER", "1")
	for _, args := range [][]string{{"api"}, {"child", "caddy", strings.Repeat("a", 32)}} {
		command, err := selfArguments(args...)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Env = os.Environ()
		body, err := cmd.CombinedOutput()
		if err == nil || !bytes.Contains(body, []byte(`"error"`)) {
			t.Fatal("unmanaged gateway entrypoint accepted", args, err)
		}
	}
	if err := c.apiProcess(strings.Repeat("a", 32)); err == nil {
		t.Fatal("unregistered API started gateway")
	}
	for _, path := range []string{c.p.socket, c.p.certificate, filepath.Join(c.p.state, "gateway.log")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("unmanaged entrypoint opened service state", path)
		}
	}
}

func TestRemoteMigrateUsesLatestEnabledChoiceAndRetainsCredentials(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(strings.ToLower(map[bool]string{true: "enabled", false: "disabled"}[enabled]), func(t *testing.T) {
			c := fixtureController(t)
			settings := testSettings()
			settings.Enabled = enabled
			if err := writePrivate(c.p.config, settings); err != nil {
				t.Fatal(err)
			}
			var actions []string
			c.serviceFn = func(action string) error { actions = append(actions, action); return nil }
			if err := c.migrate(); err != nil {
				t.Fatal(err)
			}
			after, err := loadSettings(c.p)
			if err != nil || after.Enabled != enabled || after.PasswordHash != settings.PasswordHash || after.SecretKey != settings.SecretKey {
				t.Fatal("migration changed current choice or credentials", err)
			}
			if slices.Contains(actions, "start") != enabled {
				t.Fatal("migration applied stale enabled choice", actions)
			}
		})
	}
}

func TestRemoteFixtureChildrenUseSystemOnlyToolPath(t *testing.T) {
	var childPath, productionPath string
	for _, entry := range controlEnvironment(true) {
		if strings.HasPrefix(entry, "PATH=") {
			childPath = strings.TrimPrefix(entry, "PATH=")
		}
	}
	for _, entry := range controlEnvironment(false) {
		if strings.HasPrefix(entry, "PATH=") {
			productionPath = strings.TrimPrefix(entry, "PATH=")
		}
	}
	if childPath != fixtureToolPath || strings.Contains(childPath, "homebrew") || strings.Contains(childPath, "/usr/local") {
		t.Fatal("fixture service children can discover the installed Homebrew gateway")
	}
	if productionPath != toolPath {
		t.Fatal("fixture isolation changed production tool discovery")
	}
}

func TestRemoteMigrateReadsChoiceAfterAcquiringControlLock(t *testing.T) {
	c := fixtureController(t)
	settings := testSettings()
	settings.Enabled = true
	if err := writePrivate(c.p.config, settings); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_REMOTE_TEST_HELPER", "1")
	unlock, err := c.lock("control.lock", false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	t.Setenv("AGENT_REMOTE_TEST_CONTROL_ATTEMPT", "1")
	arguments, _ := selfArguments("migrate")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, arguments[0], arguments[1:]...)
	cmd.Env = os.Environ()
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	attempted := false
	for time.Now().Before(deadline) {
		var marker struct {
			PID int `json:"pid"`
		}
		if readPrivate(filepath.Join(c.p.state, "control-attempt.json"), &marker) == nil && marker.PID == cmd.Process.Pid {
			attempted = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !attempted {
		t.Fatal("migration did not reach the held control lock")
	}
	settings.Enabled = false
	if err := writePrivate(c.p.config, settings); err != nil {
		t.Fatal(err)
	}
	unlock()
	unlock = nil
	if err := cmd.Wait(); err != nil {
		t.Fatal(err, output.String())
	}
	var status struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(output.Bytes(), &status); err != nil || status.Enabled {
		t.Fatal("migration reenabled access after concurrent disable", err)
	}
	if _, err := os.Stat(c.p.plist); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("disabled migration published a launch agent")
	}
}

func TestRemoteGatewayStartupFailureNeverPublishesReady(t *testing.T) {
	for _, scenario := range []string{"partial-ca", "occupied-port"} {
		t.Run(scenario, func(t *testing.T) {
			c := fixtureController(t)
			t.Setenv("AGENT_REMOTE_TEST_HELPER", "1")
			t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
			settings := testSettings()
			settings.Hosts = []string{"127.0.0.1"}
			settings.Enabled = true
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			settings.Port = listener.Addr().(*net.TCPAddr).Port
			var preserved map[string][]byte
			if scenario == "partial-ca" {
				listener.Close()
				preserved = makeCAAssets(t, c.p)
				if err := os.Remove(c.p.certificate); err != nil {
					t.Fatal(err)
				}
				delete(preserved, "root.crt")
			}
			if err := writePrivate(c.p.config, settings); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.disable() })
			arguments, _ := selfArguments("serve")
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, arguments[0], arguments[1:]...)
			command.Env = os.Environ()
			output, err := command.CombinedOutput()
			if err == nil || !bytes.Contains(output, []byte(`"error"`)) {
				t.Fatal("failed embedded gateway reported success", err)
			}
			if ctx.Err() != nil {
				t.Fatal("failed embedded gateway did not terminate within its deadline")
			}
			status, err := c.status()
			if err != nil || status["running"] != false || status["error"] == nil {
				t.Fatal("failed gateway published Ready or hid failure", status, err)
			}
			if _, err := os.Lstat(c.p.socket); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed gateway retained private listener", err)
			}
			for name, want := range preserved {
				got, err := os.ReadFile(filepath.Join(filepath.Dir(c.p.certificate), name))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatal("startup replaced partial CA material", name, err)
				}
			}
			if scenario == "partial-ca" {
				if _, err := os.Stat(c.p.certificate); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("startup regenerated missing trust root")
				}
			}
		})
	}
}
