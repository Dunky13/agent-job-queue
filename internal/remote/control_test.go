package remote

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fixtureController(t *testing.T) *controller {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "ar-unit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("HOME", root)
	t.Setenv("AGENT_REMOTE_TEST_MODE", "1")
	t.Setenv("AGENT_REMOTE_STATE", filepath.Join(root, "state"))
	t.Setenv("AGENT_REMOTE_CONFIG", filepath.Join(root, "config.json"))
	t.Setenv("AGENT_REMOTE_BIND", "127.0.0.1")
	p, err := newPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.prepare(); err != nil {
		t.Fatal(err)
	}
	return newController(p, context.Background())
}

func TestRemoteDependencyAndPrivateSettings(t *testing.T) {
	c := fixtureController(t)
	if !strings.Contains(toolPath, "/opt/homebrew/bin") || !strings.Contains(toolPath, "/usr/local/bin") {
		t.Fatal("missing Homebrew architecture")
	}
	settings := testSettings()
	if err := writePrivate(c.p.config, settings); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSettings(c.p)
	if err != nil || loaded.Port != settings.Port {
		t.Fatal(loaded, err)
	}
	info, _ := os.Stat(c.p.config)
	if info.Mode().Perm() != 0600 {
		t.Fatal("unsafe settings permissions")
	}
	for _, body := range []string{`[]`, `null`, `{"enabled":true}`, `{"enabled":true,"enabled":false}`} {
		_ = os.WriteFile(c.p.config, []byte(body), 0600)
		if _, err := loadSettings(c.p); err == nil {
			t.Fatal("accepted invalid settings", body)
		}
	}
	_ = writePrivate(c.p.config, settings)
	_ = os.Chmod(c.p.config, 0644)
	if _, err := loadSettings(c.p); err == nil {
		t.Fatal("accepted public settings")
	}
	_ = os.Remove(c.p.config)
	target := filepath.Join(c.p.state, "private-target")
	_ = writePrivate(target, settings)
	_ = os.Symlink(target, c.p.config)
	if _, err := loadSettings(c.p); err == nil {
		t.Fatal("followed settings symlink")
	}
	_ = os.Remove(c.p.config)
	if err := syscall.Mkfifo(c.p.config, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSettings(c.p); err == nil {
		t.Fatal("accepted settings FIFO")
	}
}

func TestRemoteConfigureRejectsInvalidInputBeforeWriting(t *testing.T) {
	c := fixtureController(t)
	_ = os.RemoveAll(c.p.state)
	for _, password := range []any{nil, 1234, "", "a", "abc", "word\n", "word\t", "word\x00", strings.Repeat("é", 37)} {
		data, _ := json.Marshal(map[string]any{"password": password})
		fields, _ := decodeObject(data)
		if err := c.configure(fields); err == nil {
			t.Fatalf("accepted password %v", password)
		}
		if _, err := os.Stat(c.p.state); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("invalid password reached configuration writes")
		}
	}
	for _, body := range []string{`{"password":"fixture","port":true}`, `{"password":"fixture","port":null}`, `{"password":"fixture","port":1023}`, `{"password":"fixture","port":65536}`, `{"password":"fixture","port":8743.0}`} {
		fields, _ := decodeObject([]byte(body))
		if err := c.configure(fields); err == nil {
			t.Fatal("accepted invalid port")
		}
	}
	for _, body := range []string{`{}`, `[]`, `null`, `{"password":"fixture","extra":true}`, `{"password":"fixture","password":"other"}`, strings.Repeat("x", 4097)} {
		if _, err := readConfigurationInput(strings.NewReader(body)); err == nil {
			t.Fatal("accepted invalid configure input")
		}
	}
}

func TestRemoteOwnershipRejectsUnknownAndRecycledProcesses(t *testing.T) {
	c := fixtureController(t)
	c.bootFn = func() (string, error) { return "fixture-boot", nil }
	const pid = 987654
	c.birthFn = func(int) (string, error) { return "fixture-birth", nil }
	path, _ := c.p.childPath(strings.Repeat("a", 32), "caddy")
	record := childRecord{PID: pid, Birth: "fixture-birth", Generation: strings.Repeat("a", 32), Role: "caddy", BootUUID: "fixture-boot"}
	if err := writePrivate(path, record); err != nil {
		t.Fatal(err)
	}
	called := false
	c.killGroupFn = func(int, syscall.Signal) error { called = true; return nil }
	c.groupFn = func(int) (int, error) { return pid + 1, nil }
	if err := c.reconcileChildren(""); err == nil || called {
		t.Fatal("signaled an unverified process group")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("lost unresolved child record")
	}
	c.groupFn = func(int) (int, error) { return 0, syscall.ESRCH }
	c.birthFn = func(int) (string, error) { return "", nil }
	if err := c.reconcileChildren(""); err != nil || called {
		t.Fatal("departed process was not removed safely", err)
	}
	if err := writePrivate(path, record); err != nil {
		t.Fatal(err)
	}
	calls := 0
	c.birthFn = func(int) (string, error) {
		calls++
		if calls < 3 {
			return "fixture-birth", nil
		}
		return "recycled", nil
	}
	c.groupFn = func(int) (int, error) { return pid, nil }
	if err := c.reconcileChildren(""); err != nil || called {
		t.Fatal("signaled recycled PID after group lookup", err)
	}
	_ = writePrivate(path, record)
	c.birthFn = func(int) (string, error) { return "fixture-birth", nil }
	c.groupFn = func(int) (int, error) { return 0, syscall.ESRCH }
	// A process can depart between birth lookup and group lookup.
	c.birthFn = func(int) (string, error) { return "", nil }
	if err := c.reconcileChildren(""); err != nil || called {
		t.Fatal("departure race failed", err)
	}
	badPath := filepath.Join(c.p.children, "unknown.json")
	_ = writePrivate(badPath, record)
	if err := c.reconcileChildren(""); err == nil || called || !c.unresolvedChildren() {
		t.Fatal("unknown record did not fail closed")
	}
}

func TestRemoteShutdownAttemptsEveryStageAndRetainsPermissionFailures(t *testing.T) {
	c := fixtureController(t)
	c.bootFn = func() (string, error) { return "fixture-boot", nil }
	c.birthFn = func(int) (string, error) { return "fixture-birth", nil }
	_ = writePrivate(c.p.pid, generationRecord{Token: strings.Repeat("b", 32), Active: true, BootUUID: "fixture-boot", Supervisor: processRecord{PID: 987654, Birth: "fixture-birth"}})
	c.serviceFn = func(string) error { return errors.New("fixture launchctl timeout") }
	c.killFn = func(int, syscall.Signal) error { return syscall.EPERM }
	reconciled := false
	c.reconcileFn = func(string) error { reconciled = true; return errors.New("fixture owned cleanup failure") }
	err := c.stopOwnedService()
	if err == nil || !strings.Contains(err.Error(), "launchctl timeout") || !strings.Contains(err.Error(), "operation not permitted") || !strings.Contains(err.Error(), "owned cleanup failure") || !reconciled {
		t.Fatal(err)
	}
	record, err := c.generation()
	if err != nil || record == nil || record.Active || record.Ready {
		t.Fatal("shutdown lost ownership or left active generation")
	}
	status, err := c.status()
	if err != nil || status["error"] == nil {
		t.Fatal("shutdown failure disappeared", status, err)
	}
	c.serviceFn = func(string) error { return nil }
	c.killFn = func(int, syscall.Signal) error { return syscall.ESRCH }
	c.reconcileFn = func(string) error { return nil }
	if err := c.disable(); err != nil {
		t.Fatal(err)
	}
	status, err = c.status()
	if err != nil || status["error"] != nil {
		t.Fatal("positive cleanup did not clear error", status, err)
	}
}

func TestRemoteSupervisorSignalRacesAndPermissions(t *testing.T) {
	for _, target := range []string{"supervisor", "test-service"} {
		t.Run(target, func(t *testing.T) {
			c := fixtureController(t)
			c.bootFn = func() (string, error) { return "boot", nil }
			c.birthFn = func(int) (string, error) { return "birth", nil }
			_ = writePrivate(c.p.pid, generationRecord{BootUUID: "boot", Supervisor: processRecord{PID: 987654, Birth: "birth"}})
			c.killFn = func(int, syscall.Signal) error { return syscall.ESRCH }
			action := c.stopSupervisor
			if target == "test-service" {
				action = func() error { return c.serviceAction("stop") }
			}
			if err := action(); err != nil {
				t.Fatal("departure race was treated as failure", err)
			}
			c.killFn = func(int, syscall.Signal) error { return syscall.EPERM }
			if err := action(); !errors.Is(err, syscall.EPERM) {
				t.Fatal("permission failure was hidden", err)
			}
		})
	}
}

func TestRemoteSupervisorRevalidatesBootAndBirthBeforeEscalation(t *testing.T) {
	for _, change := range []string{"recycled-pid", "different-boot", "birth-unavailable"} {
		t.Run(change, func(t *testing.T) {
			c := fixtureController(t)
			bootCalls, birthCalls := 0, 0
			c.bootFn = func() (string, error) {
				bootCalls++
				if change == "different-boot" && bootCalls > 1 {
					return "new-boot", nil
				}
				return "boot", nil
			}
			c.birthFn = func(int) (string, error) {
				birthCalls++
				if birthCalls > 2 {
					if change == "recycled-pid" {
						return "new-process", nil
					}
					if change == "birth-unavailable" {
						return "", errors.New("identity unavailable")
					}
				}
				return "birth", nil
			}
			if err := writePrivate(c.p.pid, generationRecord{BootUUID: "boot", Supervisor: processRecord{PID: 987654, Birth: "birth"}}); err != nil {
				t.Fatal(err)
			}
			var signals []syscall.Signal
			c.killFn = func(_ int, signal syscall.Signal) error { signals = append(signals, signal); return nil }
			err := c.stopSupervisorWithGrace(0)
			if change == "birth-unavailable" && err == nil || change != "birth-unavailable" && err != nil {
				t.Fatal(err)
			}
			if len(signals) != 1 || signals[0] != syscall.SIGTERM {
				t.Fatal("escalated against an unverified process identity", signals)
			}
		})
	}
}

// TestRemoteProcessHelper lets lifecycle fixtures exec the real controller entry
// point and its real child roles without compiling or installing another binary.
func TestRemoteProcessHelper(t *testing.T) {
	if os.Getenv("AGENT_REMOTE_TEST_HELPER") != "1" {
		return
	}
	for index, argument := range os.Args {
		if argument == "--" {
			os.Exit(Main(os.Args[index+1:]))
		}
	}
	os.Exit(125)
}

// seedLegacyCaddy is the sole external-Caddy fixture. It emulates an orphaned
// owned gateway from the previous distribution using a real legacy PKI/hash.
// The normal lifecycle suite and the embedded runtime do not invoke Caddy.
func seedLegacyCaddy(t *testing.T, c *controller, executable, password string, port int) ([]byte, []byte, *Settings) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	hashCommand := exec.CommandContext(ctx, executable, "hash-password", "--algorithm", "bcrypt", "--bcrypt-cost", "14")
	hashCommand.Env = controlEnvironment(false)
	hashCommand.Stdin = strings.NewReader(password + "\n")
	hash, err := hashCommand.Output()
	if err != nil {
		t.Fatal("legacy fixture password hash failed", err)
	}
	settings := testSettings()
	settings.Hosts = []string{"127.0.0.1"}
	settings.Port = port
	settings.PasswordHash = strings.TrimSpace(string(hash))
	if !bcryptHash.MatchString(settings.PasswordHash) {
		t.Fatal("legacy fixture hash is invalid")
	}
	if err := writePrivate(c.p.config, settings); err != nil {
		t.Fatal(err)
	}
	configuration := fmt.Sprintf("{\n admin off\n persist_config off\n auto_https disable_redirects\n skip_install_trust\n storage file_system {\n  root %s\n }\n servers {\n  protocols h1 h2\n }\n}\nhttps://127.0.0.1:%d {\n bind 127.0.0.1\n tls internal\n basic_auth {\n  agent %s\n }\n respond \"legacy gateway fixture\"\n}\n", strconv.Quote(filepath.Join(c.p.state, "caddy")), port, settings.PasswordHash)
	configPath := filepath.Join(c.p.state, "legacy-Caddyfile")
	if err := writePrivate(configPath, []byte(configuration)); err != nil {
		t.Fatal(err)
	}
	adapt := exec.CommandContext(ctx, executable, "adapt", "--config", configPath, "--adapter", "caddyfile")
	adapt.Env = controlEnvironment(false)
	adapted, err := adapt.Output()
	if err != nil {
		t.Fatal("legacy fixture adapter failed", err)
	}
	assertLegacyBasicAuthParity(t, c.p, &settings, adapted)
	logPath := filepath.Join(c.p.state, "legacy-fixture.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	process := exec.Command(executable, "run", "--config", configPath, "--adapter", "caddyfile")
	process.Env = controlEnvironment(false)
	process.Stdout = log
	process.Stderr = log
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := process.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = process.Wait(); _ = log.Close(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = process.Process.Kill()
			<-done
		}
	})
	deadline := time.Now().Add(12 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		certificate, err := os.ReadFile(c.p.certificate)
		if err == nil {
			roots := x509.NewCertPool()
			if roots.AppendCertsFromPEM(certificate) {
				transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, DisableKeepAlives: true}
				client := &http.Client{Transport: transport, Timeout: fixtureGatewayRequestBudget(t, c.p, &settings)}
				request, _ := http.NewRequest("GET", fmt.Sprintf("https://127.0.0.1:%d/", port), nil)
				request.SetBasicAuth(username, password)
				response, err := client.Do(request)
				if err == nil {
					ready = response.StatusCode == 200
					_ = response.Body.Close()
				}
				transport.CloseIdleConnections()
			}
		}
		if ready {
			break
		}
		select {
		case <-done:
			body, _ := os.ReadFile(logPath)
			t.Fatalf("legacy fixture exited: %s", body)
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		body, _ := os.ReadFile(logPath)
		t.Fatalf("legacy fixture did not become ready: %s", body)
	}
	own, err := c.record(process.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := c.bootFn()
	if err != nil {
		t.Fatal(err)
	}
	path, _ := c.p.childPath(strings.Repeat("d", 32), "caddy")
	if err := writePrivate(path, childRecord{PID: own.PID, Birth: own.Birth, Generation: strings.Repeat("d", 32), Role: "caddy", BootUUID: boot, Ack: true}); err != nil {
		t.Fatal(err)
	}
	certificate, err := os.ReadFile(c.p.certificate)
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(filepath.Join(filepath.Dir(c.p.certificate), "root.key"))
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key, &settings
}

func TestRemoteCaddyLifecycleAcceptance(t *testing.T) {
	runRemoteLifecycleAcceptance(t, "")
}

func TestRemoteLegacyCaddyMigrationAcceptance(t *testing.T) {
	executable := os.Getenv("AGENT_REMOTE_TEST_LEGACY_CADDY")
	if executable == "" {
		t.Skip("set AGENT_REMOTE_TEST_LEGACY_CADDY to an explicitly authorized legacy Caddy executable for temporary compatibility seeding")
	}
	if !filepath.IsAbs(executable) {
		t.Fatal("legacy fixture executable must be absolute")
	}
	runRemoteLifecycleAcceptance(t, executable)
}

func runRemoteLifecycleAcceptance(t *testing.T, legacyExecutable string) {
	if testing.Short() {
		t.Skip("real private Caddy lifecycle acceptance")
	}
	c := fixtureController(t)
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
	if _, err := exec.LookPath("caddy"); err == nil {
		t.Fatal("embedded acceptance PATH contains an external Caddy executable")
	}
	t.Setenv("AGENT_REMOTE_TEST_HELPER", "1")
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	controllerPath := filepath.Join(c.p.state, "controller")
	callLog := filepath.Join(c.p.state, "calls.jsonl")
	controllerScript := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + callLog + "'\nif [ \"$1\" = status ]; then printf '%s' '{\"jobs\":[],\"queue\":{\"limit\":1}}'; else printf '%s' '{\"ok\":true}'; fi\n"
	if err := os.WriteFile(controllerPath, []byte(controllerScript), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_REMOTE_TEST_CONTROLLER", controllerPath)
	cli := func(t *testing.T, input string, args ...string) []byte {
		t.Helper()
		command, err := selfArguments(args...)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, command[0], command[1:]...)
		cmd.Env = os.Environ()
		cmd.Stdin = strings.NewReader(input)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("remote %s: %v %s", args[0], err, output)
		}
		return output
	}
	password := "2468"
	configure := func(t *testing.T, value string) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"password": value, "port": port})
		cli(t, string(data), "configure")
	}
	var legacyCertificate, legacyKey []byte
	var legacySettings *Settings
	if legacyExecutable == "" {
		configure(t, password)
	} else {
		legacyCertificate, legacyKey, legacySettings = seedLegacyCaddy(t, c, legacyExecutable, password, port)
	}
	cli(t, "", "enable")
	var process *exec.Cmd
	var exited chan struct{}
	processLog := filepath.Join(c.p.state, "supervisor-test.log")
	start := func(t *testing.T, gate string) {
		t.Helper()
		command, _ := selfArguments("serve")
		process = exec.Command(command[0], command[1:]...)
		process.Env = os.Environ()
		if gate != "" {
			process.Env = append(process.Env, "AGENT_REMOTE_TEST_ACK_GATE="+gate)
		}
		log, err := os.OpenFile(processLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		process.Stdout = log
		process.Stderr = log
		if err := process.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}
		exited = make(chan struct{})
		current := process
		done := exited
		go func() { _ = current.Wait(); _ = log.Close(); close(done) }()
	}
	waitExit := func(t *testing.T) {
		t.Helper()
		select {
		case <-exited:
		case <-time.After(8 * time.Second):
			t.Fatal("isolated supervisor did not exit")
		}
	}
	portOpen := func() bool {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 150*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}
	waitReady := func(t *testing.T) {
		t.Helper()
		deadline := time.Now().Add(18 * time.Second)
		for time.Now().Before(deadline) {
			if c.isRunning() {
				return
			}
			select {
			case <-exited:
				data, _ := os.ReadFile(processLog)
				service, _ := os.ReadFile(c.p.log)
				t.Fatalf("fixture supervisor exited: %s %s", data, service)
			default:
			}
			time.Sleep(100 * time.Millisecond)
		}
		data, _ := os.ReadFile(processLog)
		service, _ := os.ReadFile(c.p.log)
		t.Fatalf("fixture was not ready: %s %s", data, service)
	}
	t.Cleanup(func() {
		_ = c.disable()
		if process != nil {
			select {
			case <-exited:
			default:
				_ = process.Process.Kill()
				<-exited
			}
		}
	})
	start(t, "")
	waitReady(t)
	url := fmt.Sprintf("https://127.0.0.1:%d", port)
	certificate, err := os.ReadFile(c.p.certificate)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificate) {
		t.Fatal("invalid fixture certificate")
	}
	rootKey, err := os.ReadFile(filepath.Join(filepath.Dir(c.p.certificate), "root.key"))
	if err != nil {
		t.Fatal(err)
	}
	initialSettings, err := loadSettings(c.p)
	if err != nil {
		t.Fatal(err)
	}
	requestBudget := fixtureGatewayRequestBudget(t, c.p, initialSettings)
	if legacyExecutable != "" && (!bytes.Equal(certificate, legacyCertificate) || !bytes.Equal(rootKey, legacyKey) || initialSettings.PasswordHash != legacySettings.PasswordHash || initialSettings.SecretKey != legacySettings.SecretKey) {
		t.Fatal("embedded migration replaced legacy phone trust or credentials")
	}
	assertTrust := func(t *testing.T) {
		t.Helper()
		current, err := os.ReadFile(c.p.certificate)
		if err != nil || !bytes.Equal(current, certificate) {
			t.Fatal("phone trust root changed", err)
		}
		current, err = os.ReadFile(filepath.Join(filepath.Dir(c.p.certificate), "root.key"))
		if err != nil || !bytes.Equal(current, rootKey) {
			t.Fatal("phone trust root key changed", err)
		}
	}
	assertCredentials := func(t *testing.T) {
		t.Helper()
		settings, err := loadSettings(c.p)
		if err != nil || settings.PasswordHash != initialSettings.PasswordHash || settings.SecretKey != initialSettings.SecretKey {
			t.Fatal("restart changed stored credentials", err)
		}
	}
	paths, err := c.childRecords()
	if err != nil || len(paths) != 1 {
		t.Fatal("embedded service must own exactly one API child", paths, err)
	}
	var soleChild childRecord
	if readPrivate(paths[0], &soleChild) != nil || soleChild.Role != "api" || !soleChild.Ready {
		t.Fatal("embedded gateway is not ready in its owned API child")
	}
	newClient := func(trusted bool) *http.Client {
		jar, _ := cookiejar.New(nil)
		var pool *x509.CertPool
		if trusted {
			pool = roots
		}
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
		t.Cleanup(transport.CloseIdleConnections)
		return &http.Client{Transport: transport, Jar: jar, Timeout: requestBudget}
	}
	getCSRF := func(t *testing.T, client *http.Client, credential string) (string, int) {
		t.Helper()
		request, _ := http.NewRequest("GET", url+"/api/csrf", nil)
		if credential != "" {
			request.SetBasicAuth(username, credential)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		var token struct {
			Token string `json:"token"`
		}
		_ = json.Unmarshal(data, &token)
		return token.Token, response.StatusCode
	}
	t.Run("TLS authentication private state and CSRF", func(t *testing.T) {
		request, _ := http.NewRequest("GET", url+"/api/csrf", nil)
		if response, err := newClient(false).Do(request); err == nil {
			response.Body.Close()
			t.Fatal("untrusted certificate accepted")
		}
		for _, credential := range []string{"", "wrong-fixture-password"} {
			if _, status := getCSRF(t, newClient(true), credential); status != 401 {
				t.Fatalf("unexpected unauthenticated status %d", status)
			}
		}
		client := newClient(true)
		token, status := getCSRF(t, client, password)
		if status != 200 || token == "" {
			t.Fatal(status, token)
		}
		for _, origin := range []string{url, "https://evil.test"} {
			request, _ := http.NewRequest("POST", url+"/api/limit", bytes.NewBufferString(`{"limit":2}`))
			request.SetBasicAuth(username, password)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Origin", origin)
			request.Header.Set("Referer", url+"/")
			request.Header.Set("X-CSRFToken", token)
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if origin == url && response.StatusCode != 200 || origin != url && response.StatusCode != 403 {
				t.Fatal(response.StatusCode, string(data))
			}
		}
		data, _ := os.ReadFile(callLog)
		if !strings.Contains(string(data), "limit 2") {
			t.Fatal("Caddy request did not reach private controller")
		}
		config, _ := os.ReadFile(c.p.config)
		if bytes.Contains(config, []byte(password)) || !bytes.Contains(config, []byte("$2a$14$")) {
			t.Fatal("configuration does not contain only the expected hash")
		}
		for _, path := range []string{c.p.config, c.p.socket} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("private path permissions", path, err)
			}
		}
		gateway, _ := os.ReadFile(c.p.gatewayConfig)
		for _, setting := range []string{`"disabled":true`, `"persist":false`, `"install_trust":false`, `"Authorization"`, `"Forwarded"`} {
			if !bytes.Contains(gateway, []byte(setting)) {
				t.Fatal("missing Caddy boundary", setting)
			}
		}
	})
	t.Run("HTTP2 uses the same trusted authenticated gateway", func(t *testing.T) {
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: true}
		defer transport.CloseIdleConnections()
		request, _ := http.NewRequest("GET", url+"/api/csrf", nil)
		request.SetBasicAuth(username, password)
		response, err := (&http.Client{Transport: transport, Timeout: requestBudget}).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var payload struct {
			Token string `json:"token"`
		}
		if response.ProtoMajor != 2 || response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&payload) != nil || payload.Token == "" {
			t.Fatal("HTTP2 did not reach the authenticated private API", response.Proto, response.StatusCode)
		}
	})
	t.Run("SIGKILL restart reconciles owned children", func(t *testing.T) {
		paths, _ := c.childRecords()
		var previous []childRecord
		for _, path := range paths {
			var record childRecord
			if err := readPrivate(path, &record); err != nil {
				t.Fatal(err)
			}
			previous = append(previous, record)
		}
		if err := process.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		waitExit(t)
		if c.isRunning() {
			t.Fatal("dead supervisor published ready")
		}
		start(t, "")
		waitReady(t)
		for _, record := range previous {
			if birth, _ := c.birthFn(record.PID); birth == record.Birth {
				t.Fatal("old service child survived restart")
			}
		}
		if _, status := getCSRF(t, newClient(true), password); status != 200 {
			t.Fatal(status)
		}
		assertTrust(t)
		assertCredentials(t)
	})
	t.Run("SIGKILL before acknowledgement cannot open listener", func(t *testing.T) {
		cli(t, "", "disable")
		waitExit(t)
		if portOpen() {
			t.Fatal("gateway survived disable")
		}
		cli(t, "", "enable")
		gate := filepath.Join(c.p.state, "ack-gate")
		start(t, gate)
		deadline := time.Now().Add(8 * time.Second)
		registered := false
		for time.Now().Before(deadline) {
			matches, _ := filepath.Glob(filepath.Join(c.p.children, "*-api.json"))
			if len(matches) != 0 {
				var record childRecord
				if err := readPrivate(matches[0], &record); err == nil {
					if record.Ack {
						t.Fatal("fixture gate acknowledged child")
					}
					registered = true
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
		}
		_, socketErr := os.Lstat(c.p.socket)
		if !registered || portOpen() || socketErr == nil {
			t.Fatal("unacknowledged child opened gateway")
		}
		_ = process.Process.Kill()
		waitExit(t)
		cli(t, "", "disable")
		_ = os.WriteFile(gate, nil, 0600)
		time.Sleep(150 * time.Millisecond)
		paths, _ := c.childRecords()
		if portOpen() || len(paths) != 0 {
			t.Fatal("pre-ack shutdown lost child ownership")
		}
		cli(t, "", "enable")
		start(t, "")
		waitReady(t)
		assertTrust(t)
		assertCredentials(t)
	})
	t.Run("enabled reconfigure rotates credentials and session", func(t *testing.T) {
		configure(t, "replacement-fixture-password")
		waitExit(t)
		if portOpen() {
			t.Fatal("old credentials remained listening")
		}
		start(t, "")
		waitReady(t)
		if _, status := getCSRF(t, newClient(true), password); status != 401 {
			t.Fatal("old password accepted", status)
		}
		password = "replacement-fixture-password"
		if _, status := getCSRF(t, newClient(true), password); status != 200 {
			t.Fatal("replacement password rejected", status)
		}
		rotated, err := loadSettings(c.p)
		if err != nil || rotated.PasswordHash == initialSettings.PasswordHash || rotated.SecretKey == initialSettings.SecretKey {
			t.Fatal("configure did not rotate password and session keys", err)
		}
		assertTrust(t)
	})
	t.Run("launchctl failure still closes owned gateway", func(t *testing.T) {
		c.serviceFn = func(string) error { return errors.New("fixture launchctl timeout") }
		if err := c.disable(); err == nil || !strings.Contains(err.Error(), "shutdown reported errors") {
			t.Fatal(err)
		}
		waitExit(t)
		if portOpen() {
			t.Fatal("tool failure left gateway open")
		}
		status, err := c.status()
		if err != nil || status["enabled"] != false || status["error"] == nil {
			t.Fatal(status, err)
		}
		c.serviceFn = c.serviceAction
		cli(t, "", "disable")
		status, _ = c.status()
		if status["error"] != nil {
			t.Fatal("recovered disable retained error")
		}
		cli(t, "", "enable")
		start(t, "")
		waitReady(t)
	})
	t.Run("incomplete shutdown remains visible until positive cleanup", func(t *testing.T) {
		_ = process.Process.Kill()
		waitExit(t)
		c.serviceFn = func(string) error { return errors.New("fixture stop tool failure") }
		c.reconcileFn = func(string) error { return errors.New("fixture owned cleanup failure") }
		if err := c.disable(); err == nil || !strings.Contains(err.Error(), "owned cleanup failure") {
			t.Fatal(err)
		}
		if !portOpen() {
			t.Fatal("fixture did not preserve unresolved owned listener")
		}
		status, _ := c.status()
		if status["enabled"] != false || status["running"] != false || status["error"] == nil {
			t.Fatal(status)
		}
		c.serviceFn = c.serviceAction
		c.reconcileFn = c.reconcileChildren
		cli(t, "", "disable")
		if portOpen() {
			t.Fatal("positive cleanup left gateway open")
		}
		paths, _ := c.childRecords()
		if len(paths) != 0 {
			t.Fatal("child ownership was not reconciled")
		}
		status, _ = c.status()
		if status["error"] != nil {
			t.Fatal(status)
		}
	})
}
