package remote

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The release smoke script supplies an extracted, signed payload. Execute that
// actual universal binary rather than the test executable's embedded gateway.
func TestRemotePackagedGateway(t *testing.T) {
	payload := os.Getenv("AGENT_QUEUE_TEST_INSTALLER_PAYLOAD")
	if payload == "" {
		t.Skip("release payload not supplied")
	}
	if !filepath.IsAbs(payload) {
		t.Fatal("release payload must be absolute")
	}
	executable := filepath.Join(payload, "source/bin/agent-remote")
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		t.Fatal("packaged remote executable is unavailable", err)
	}
	c := fixtureController(t)
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
	t.Setenv("AGENT_REMOTE_TEST_HELPER", "")
	if _, err := exec.LookPath("caddy"); err == nil {
		t.Fatal("packaged gateway fixture PATH contains external Caddy")
	}
	controller := filepath.Join(c.p.state, "fixture-controller")
	if err := os.WriteFile(controller, []byte("#!/bin/sh\nprintf '{\"jobs\":[],\"queue\":{\"limit\":1},\"fixturePath\":\"%s\"}' \"$PATH\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_REMOTE_TEST_CONTROLLER", controller)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	command := func(ctx context.Context, input []byte, arguments ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, executable, arguments...)
		cmd.Env = os.Environ()
		cmd.Stdin = bytes.NewReader(input)
		cmd.WaitDelay = time.Second
		return cmd.CombinedOutput()
	}
	password := "packaged-gateway-fixture"
	input, err := json.Marshal(map[string]any{"password": password, "port": port})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	output, err := command(ctx, input, "configure")
	cancel()
	if err != nil {
		t.Fatal("packaged configure failed", err, string(output))
	}
	settings, err := loadSettings(c.p)
	if err != nil || settings == nil || settings.Enabled {
		t.Fatal("packaged configure did not leave remote access disabled", err)
	}
	settings.Enabled = true
	if err := writePrivate(c.p.config, settings); err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(filepath.Join(c.p.state, "packaged-supervisor.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	process := exec.Command(executable, "serve")
	process.Env = os.Environ()
	process.Stdout = log
	process.Stderr = log
	if err := process.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	var processErr error
	go func() { processErr = process.Wait(); _ = log.Close(); close(done) }()
	owned, identityErr := c.record(process.Process.Pid)
	boot, bootErr := c.bootFn()
	// Always retire only this fixture's positively identified supervisor and
	// children, including after an assertion failure. No installed service is used.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		_, disableErr := command(ctx, nil, "disable")
		cancel()
		if disableErr != nil {
			if err := c.disable(); err != nil {
				t.Errorf("packaged fixture owned cleanup failed: %v", err)
			}
		}
		select {
		case <-done:
			return
		case <-time.After(3 * time.Second):
		}
		for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
			if identityErr != nil || bootErr != nil {
				t.Error("packaged fixture process identity unavailable; refusing an unverified signal")
				return
			}
			currentBoot, err := c.bootFn()
			if err != nil || currentBoot != boot {
				t.Error("packaged fixture boot identity changed; refusing an unverified signal")
				return
			}
			birth, err := c.birthFn(owned.PID)
			if err != nil {
				t.Error("packaged fixture process identity could not be rechecked")
				return
			}
			if birth != owned.Birth {
				break
			}
			if err := c.killFn(owned.PID, signal); err != nil && err != syscall.ESRCH {
				t.Errorf("packaged fixture process stop failed: %v", err)
				return
			}
			select {
			case <-done:
				if err := c.stopOwnedService(); err != nil {
					t.Errorf("packaged fixture child reconciliation failed: %v", err)
				}
				return
			case <-time.After(3 * time.Second):
			}
		}
		select {
		case <-done:
		default:
			t.Error("packaged fixture supervisor exit was not observed")
		}
	})
	if identityErr != nil || bootErr != nil {
		t.Fatal("could not establish packaged fixture ownership", identityErr, bootErr)
	}
	deadline := time.Now().Add(18 * time.Second)
	for !c.isRunning() {
		select {
		case <-done:
			body, _ := os.ReadFile(filepath.Join(c.p.state, "packaged-supervisor.log"))
			service, _ := os.ReadFile(c.p.log)
			t.Fatalf("packaged remote exited before readiness: %v %s %s", processErr, body, service)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("packaged embedded gateway did not become ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
	paths, err := c.childRecords()
	if err != nil || len(paths) != 1 {
		t.Fatal("packaged gateway must own one API child", paths, err)
	}
	var child childRecord
	if err := readPrivate(paths[0], &child); err != nil || child.Role != "api" || !child.Ack || !child.Ready {
		t.Fatal("packaged gateway child is not the acknowledged ready API", err)
	}
	generation, err := c.generation()
	if err != nil || generation == nil || generation.Gateway != embeddedGateway || generation.Supervisor.PID != process.Process.Pid {
		t.Fatal("packaged service did not establish embedded gateway ownership", err)
	}
	gateway, err := os.ReadFile(c.p.gatewayConfig)
	if err != nil || !bytes.Contains(gateway, []byte(`"hash_cache":{}`)) {
		t.Fatal("packaged gateway omitted Caddy's bounded auth cache", err)
	}
	certificate, err := os.ReadFile(c.p.certificate)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificate) {
		t.Fatal("packaged local CA certificate is invalid")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: fixtureGatewayRequestBudget(t, c.p, settings)}
	url := "https://" + listener.Addr().String()
	request, _ := http.NewRequest("GET", url+"/api/csrf", nil)
	request.SetBasicAuth(username, password)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("packaged trusted authenticated HTTPS failed", err)
	}
	var token struct {
		Token string `json:"token"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&token)
	_ = response.Body.Close()
	if response.ProtoMajor != 2 || response.StatusCode != 200 || decodeErr != nil || token.Token == "" {
		t.Fatal("packaged HTTP2 did not reach the authenticated private API", response.Proto, response.StatusCode, decodeErr)
	}
	cookies := response.Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("packaged gateway CSRF cookie is not private and secure")
	}
	request, _ = http.NewRequest("GET", url+"/api/status", nil)
	request.SetBasicAuth(username, password)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var fixtureStatus struct {
		FixturePath string            `json:"fixturePath"`
		Jobs        []json.RawMessage `json:"jobs"`
	}
	decodeErr = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&fixtureStatus)
	_ = response.Body.Close()
	if response.StatusCode != 200 || decodeErr != nil || fixtureStatus.Jobs == nil || fixtureStatus.FixturePath != fixtureToolPath || strings.Contains(fixtureStatus.FixturePath, "homebrew") || strings.Contains(fixtureStatus.FixturePath, "/usr/local") {
		t.Fatal("packaged API child did not retain the isolated no-Caddy environment", decodeErr)
	}
	request, _ = http.NewRequest("GET", url+"/api/csrf", nil)
	request.SetBasicAuth(username, "wrong-packaged-fixture")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("packaged gateway accepted a wrong password", response.StatusCode)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 35*time.Second)
	output, err = command(ctx, nil, "disable")
	cancel()
	if err != nil {
		t.Fatal("packaged disable failed", err, string(output))
	}
	select {
	case <-done:
		if processErr != nil {
			t.Fatal("packaged supervisor did not stop cleanly", processErr)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("packaged supervisor did not exit after disable")
	}
	paths, err = c.childRecords()
	if err != nil || len(paths) != 0 {
		t.Fatal("packaged disable retained service children", err)
	}
	if _, err := os.Lstat(c.p.socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("packaged disable retained its private listener", err)
	}
	connection, err := net.DialTimeout("tcp", listener.Addr().String(), 200*time.Millisecond)
	if err == nil {
		connection.Close()
		t.Fatal("packaged disable left its HTTPS listener open")
	}
}
