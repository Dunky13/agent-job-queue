package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
	"github.com/caddyserver/caddy/v2"
	"golang.org/x/crypto/bcrypt"
)

const toolPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
const fixtureToolPath = "/usr/bin:/bin:/usr/sbin:/sbin"
const label = "io.went.agent-remote"
const username = "agent"
const defaultPort = 8743
const remoteUsage = "Usage: agent-remote status [--json] | configure | enable | disable | serve"

// Settings is the durable, backwards-compatible remote configuration. Passwords
// use bcrypt hashes and are never accepted through command line arguments.
type Settings struct {
	Enabled       bool                   `json:"enabled"`
	Username      string                 `json:"username"`
	Port          int                    `json:"port"`
	Hosts         []string               `json:"hosts"`
	PasswordHash  string                 `json:"password_hash"`
	SecretKey     string                 `json:"secret_key"`
	AuthAdmission *AuthAdmissionSettings `json:"auth_admission,omitempty"`
}

type remotePaths struct {
	testing                                                                                 bool
	state, config, plist, socket, pid, gatewayConfig, certificate, log, errorFile, children string
	home, managedApp, managedCandidate                                                      string
}

func newPaths() (remotePaths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return remotePaths{}, err
	}
	p := remotePaths{testing: os.Getenv("AGENT_REMOTE_TEST_MODE") == "1", home: home}
	p.state = filepath.Join(home, ".local/state/agent-heavy/remote")
	p.config = filepath.Join(home, ".config/agent-heavy/remote.json")
	p.plist = filepath.Join(home, "Library/LaunchAgents", label+".plist")
	if p.testing {
		if value := os.Getenv("AGENT_REMOTE_STATE"); value != "" {
			p.state = value
		}
		p.config = filepath.Join(p.state, "config.json")
		if value := os.Getenv("AGENT_REMOTE_CONFIG"); value != "" {
			p.config = value
		}
		p.plist = filepath.Join(p.state, label+".plist")
	}
	for _, name := range []string{p.state, p.config, p.plist} {
		if !filepath.IsAbs(name) {
			return remotePaths{}, errors.New("Remote paths must be absolute")
		}
	}
	p.socket = filepath.Join(p.state, "api.sock")
	p.pid = filepath.Join(p.state, "processes.json")
	p.gatewayConfig = filepath.Join(p.state, "gateway.json")
	p.certificate = filepath.Join(p.state, "caddy/pki/authorities/local/root.crt")
	p.log = filepath.Join(p.state, "service.log")
	p.errorFile = filepath.Join(p.state, "error.json")
	p.children = filepath.Join(p.state, "children")
	if !p.testing {
		installed, err := appservices.Load(home)
		if err != nil {
			return remotePaths{}, fmt.Errorf("Cannot read managed phone service ownership: %w", err)
		}
		if installed != nil {
			p.managedApp = installed.App
		}
		executable, err := os.Executable()
		if err != nil {
			return remotePaths{}, err
		}
		p.managedCandidate, _ = appservices.BundleForExecutable(executable)
	}
	return p, nil
}

func (p remotePaths) prepare() error {
	for _, directory := range []string{p.state, filepath.Dir(p.config), filepath.Join(p.state, "caddy"), p.children} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
		info, err := os.Lstat(directory)
		if err != nil {
			return err
		}
		if !info.IsDir() || !ownedByCurrentUser(info) {
			return errors.New("Remote access requires private directories owned by the current user")
		}
		if err := os.Chmod(directory, 0700); err != nil {
			return err
		}
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}

func writePrivate(path string, value any) error {
	var data []byte
	var err error
	switch v := value.(type) {
	case []byte:
		data = v
	default:
		data, err = json.Marshal(value)
	}
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}

func readPrivate(path string, target any) error {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !ownedByCurrentUser(info) || info.Mode().Perm()&0077 != 0 {
		return errors.New("Unsafe remote settings ownership or permissions")
	}
	if info.Size() > 1<<20 {
		return errors.New("Remote settings are too large")
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("Remote settings are too large")
	}
	if _, err := decodeObject(data); err != nil {
		return errors.New("Invalid remote settings")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

var bcryptHash = regexp.MustCompile(`^\$2[aby]\$14\$[./A-Za-z0-9]{53}$`)
var hostPattern = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
var localHostPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
var tokenPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func loadSettings(p remotePaths) (*Settings, error) {
	var raw map[string]json.RawMessage
	if err := readPrivate(p.config, &raw); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if !keysAre(raw, "enabled", "username", "port", "hosts", "password_hash", "secret_key") && !keysAre(raw, "enabled", "username", "port", "hosts", "password_hash", "secret_key", "auth_admission") {
		return nil, errors.New("Invalid remote settings fields")
	}
	for _, name := range []string{"enabled", "username", "port", "hosts", "password_hash", "secret_key"} {
		if bytes.Equal(raw[name], []byte("null")) {
			return nil, errors.New("Invalid remote settings")
		}
	}
	admission, err := parseAuthAdmission(raw["auth_admission"])
	if err != nil {
		return nil, err
	}
	data, _ := json.Marshal(raw)
	var settings Settings
	if json.Unmarshal(data, &settings) != nil || settings.Username != username || settings.Port < 1024 || settings.Port > 65535 {
		return nil, errors.New("Invalid remote settings")
	}
	if !bcryptHash.MatchString(settings.PasswordHash) || len(settings.SecretKey) < 32 {
		return nil, errors.New("Invalid remote credential configuration")
	}
	if len(settings.Hosts) == 0 {
		return nil, errors.New("Invalid remote host configuration")
	}
	for _, host := range settings.Hosts {
		if !hostPattern.MatchString(host) {
			return nil, errors.New("Invalid remote host configuration")
		}
	}
	settings.AuthAdmission = admission
	return &settings, nil
}

type processRecord struct {
	PID   int    `json:"pid"`
	Birth string `json:"birth"`
}
type generationRecord struct {
	Token      string        `json:"token"`
	Active     bool          `json:"active"`
	Ready      bool          `json:"ready"`
	BootUUID   string        `json:"boot_uuid"`
	Revision   string        `json:"revision"`
	Supervisor processRecord `json:"supervisor"`
	Gateway    string        `json:"gateway,omitempty"`
}
type childRecord struct {
	PID        int    `json:"pid"`
	Birth      string `json:"birth"`
	Generation string `json:"generation"`
	Role       string `json:"role"`
	BootUUID   string `json:"boot_uuid"`
	Ack        bool   `json:"ack"`
	Ready      bool   `json:"ready,omitempty"`
}

type controller struct {
	p                remotePaths
	ctx              context.Context
	birthFn          func(int) (string, error)
	bootFn           func() (string, error)
	groupFn          func(int) (int, error)
	killFn           func(int, syscall.Signal) error
	killGroupFn      func(int, syscall.Signal) error
	serviceFn        func(string) error
	reconcileFn      func(string) error
	managedCommand   func([]string) ([]byte, error)
	newCommand       func(context.Context, string, ...string) *exec.Cmd
	installMigration bool
}

func newController(p remotePaths, ctx context.Context) *controller {
	c := &controller{p: p, ctx: ctx, groupFn: syscall.Getpgid, killFn: syscall.Kill, killGroupFn: func(pid int, signal syscall.Signal) error { return syscall.Kill(-pid, signal) }}
	c.birthFn = c.processBirth
	c.bootFn = c.bootIdentity
	c.serviceFn = c.serviceAction
	c.reconcileFn = c.reconcileChildren
	return c
}

func controlEnvironment(testing bool) []string {
	home, _ := os.UserHomeDir()
	path := toolPath
	if testing {
		path = fixtureToolPath
	}
	env := []string{"HOME=" + home, "PATH=" + path, "GOMAXPROCS=2"}
	if testing {
		for _, name := range []string{"AGENT_REMOTE_TEST_MODE", "AGENT_REMOTE_STATE", "AGENT_REMOTE_CONFIG", "AGENT_REMOTE_BIND", "AGENT_REMOTE_TEST_CONTROLLER", "AGENT_REMOTE_TEST_HELPER", "AGENT_REMOTE_TEST_ACK_GATE", "AGENT_REMOTE_TEST_CONTROL_ATTEMPT"} {
			if value, ok := os.LookupEnv(name); ok {
				env = append(env, name+"="+value)
			}
		}
	}
	return env
}

func (c *controller) command(arguments []string, input []byte, timeout time.Duration, check bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(c.ctx, timeout)
	defer cancel()
	newCommand := c.newCommand
	if newCommand == nil {
		newCommand = exec.CommandContext
	}
	cmd := newCommand(ctx, arguments[0], arguments[1:]...)
	cmd.WaitDelay = time.Second
	cmd.Env = controlEnvironment(false)
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	output := &boundedBuffer{limit: 1 << 20}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, errors.New("A required remote-access tool is unavailable or exceeded its deadline")
	}
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			return nil, errors.New("A required remote-access tool is unavailable")
		}
		if check {
			return nil, errors.New("Remote-access tool failed; inspect the private service log")
		}
	}
	return output.Bytes(), nil
}

func (c *controller) lanAddress() (string, error) {
	if c.p.testing && os.Getenv("AGENT_REMOTE_BIND") != "" {
		ip := net.ParseIP(os.Getenv("AGENT_REMOTE_BIND"))
		if ip == nil || ip.To4() == nil {
			return "", errors.New("Invalid test bind address")
		}
		return ip.String(), nil
	}
	interfaces := []string{}
	route, err := c.command([]string{"/sbin/route", "-n", "get", "default"}, nil, 5*time.Second, false)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(route))
	for i, field := range fields {
		if field == "interface:" && i+1 < len(fields) {
			interfaces = append(interfaces, fields[i+1])
			break
		}
	}
	output, err := c.command([]string{"/sbin/ifconfig", "-l"}, nil, 5*time.Second, true)
	if err != nil {
		return "", err
	}
	interfaces = append(interfaces, strings.Fields(string(output))...)
	seen := map[string]bool{}
	for _, name := range interfaces {
		if seen[name] {
			continue
		}
		seen[name] = true
		output, err := c.command([]string{"/usr/sbin/ipconfig", "getifaddr", name}, nil, 5*time.Second, false)
		if err != nil {
			return "", err
		}
		ip := net.ParseIP(strings.TrimSpace(string(output)))
		if ip != nil && ip.To4() != nil && ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			return ip.String(), nil
		}
	}
	return "", errors.New("No private LAN IPv4 address is available")
}

func (c *controller) allowedHosts() ([]string, error) {
	ip, err := c.lanAddress()
	if err != nil {
		return nil, err
	}
	hosts := []string{ip}
	if !c.p.testing {
		name, err := c.command([]string{"/usr/sbin/scutil", "--get", "LocalHostName"}, nil, 5*time.Second, false)
		if err != nil {
			return nil, err
		}
		value := strings.TrimSpace(string(name))
		if localHostPattern.MatchString(value) {
			hosts = append(hosts, strings.ToLower(value)+".local")
		}
	}
	return hosts, nil
}

func (c *controller) processBirth(pid int) (string, error) {
	if pid <= 1 {
		return "", nil
	}
	output, err := c.command([]string{"/bin/ps", "-p", strconv.Itoa(pid), "-o", "lstart=,stat="}, nil, 5*time.Second, false)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(output))
	boundary := strings.LastIndexAny(value, " \t")
	if boundary < 0 || strings.HasPrefix(value[boundary+1:], "Z") {
		return "", nil
	}
	// Keep ps's internal spacing so pre-migration Python process records remain
	// recognizable during an upgrade and can be stopped safely.
	return strings.TrimSpace(value[:boundary]), nil
}

func (c *controller) bootIdentity() (string, error) {
	var output []byte
	var err error
	if runtime.GOOS == "darwin" {
		output, err = c.command([]string{"/usr/sbin/sysctl", "-n", "kern.bootsessionuuid"}, nil, 5*time.Second, true)
	} else {
		output, err = os.ReadFile("/proc/sys/kernel/random/boot_id")
	}
	if err != nil {
		return "", err
	}
	value := strings.ToLower(strings.TrimSpace(string(output)))
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(value) {
		return "", errors.New("Could not establish boot identity")
	}
	return value, nil
}

func (c *controller) record(pid int) (processRecord, error) {
	birth, err := c.birthFn(pid)
	if err != nil {
		return processRecord{}, err
	}
	if birth == "" {
		return processRecord{}, errors.New("Could not establish service process identity")
	}
	return processRecord{PID: pid, Birth: birth}, nil
}

func revision(settings *Settings) string {
	data, _ := json.Marshal(settings)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (c *controller) lock(name string, nonblocking bool) (func(), error) {
	name = filepath.Join(c.p.state, name)
	fd, err := syscall.Open(name, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !ownedByCurrentUser(info) || info.Mode().Perm()&0077 != 0 {
		f.Close()
		return nil, errors.New("Unsafe remote coordination lock")
	}
	deadline := time.Now().Add(5 * time.Second)
	if c.p.testing && filepath.Base(name) == "control.lock" && os.Getenv("AGENT_REMOTE_TEST_CONTROL_ATTEMPT") == "1" {
		if err := writePrivate(filepath.Join(c.p.state, "control-attempt.json"), map[string]any{"pid": os.Getpid()}); err != nil {
			f.Close()
			return nil, err
		}
	}
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			f.Close()
			return nil, err
		}
		if nonblocking || time.Now().After(deadline) || c.ctx.Err() != nil {
			f.Close()
			return nil, errors.New("Remote service registry is busy")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *controller) generation() (*generationRecord, error) {
	var record generationRecord
	if err := readPrivate(c.p.pid, &record); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return &record, nil
}

func (p remotePaths) childPath(token, role string) (string, error) {
	if !tokenPattern.MatchString(token) || role != "api" && role != "caddy" {
		return "", errors.New("Invalid service child identity")
	}
	return filepath.Join(p.children, token+"-"+role+".json"), nil
}

func (c *controller) isRunning() bool {
	record, err := c.generation()
	if err != nil || record == nil || !record.Active || !record.Ready || record.Gateway != embeddedGateway {
		return false
	}
	settings, err := loadSettings(c.p)
	if err != nil || settings == nil || !settings.Enabled || record.Revision != revision(settings) {
		return false
	}
	boot, err := c.bootFn()
	if err != nil || boot != record.BootUUID {
		return false
	}
	started, err := c.birthFn(record.Supervisor.PID)
	if err != nil || record.Supervisor.Birth == "" || started != record.Supervisor.Birth {
		return false
	}
	for _, role := range []string{"api"} {
		path, err := c.p.childPath(record.Token, role)
		if err != nil {
			return false
		}
		var child childRecord
		if readPrivate(path, &child) != nil || !child.Ack || !child.Ready || child.Birth == "" || child.Generation != record.Token || child.Role != role || child.BootUUID != boot {
			return false
		}
		birth, err := c.birthFn(child.PID)
		if err != nil || birth != child.Birth {
			return false
		}
	}
	info, err := os.Lstat(c.p.socket)
	return err == nil && info.Mode()&os.ModeSocket != 0 && ownedByCurrentUser(info) && info.Mode().Perm()&0077 == 0
}

func (c *controller) status() (map[string]any, error) {
	settings, err := loadSettings(c.p)
	if err != nil {
		return nil, err
	}
	enabled := settings != nil && settings.Enabled
	running := c.isRunning()
	var failure map[string]any
	err = readPrivate(c.p.errorFile, &failure)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var errorValue any = nil
	if failure != nil {
		errorValue = failure["error"]
	}
	if !enabled && c.unresolvedChildren() && errorValue == nil {
		errorValue = "Remote access is disabled, but owned service cleanup is incomplete"
	} else if enabled && !running && errorValue == nil {
		errorValue = "Remote service is not running"
	}
	url, port, cert := "", defaultPort, ""
	if settings != nil {
		url = fmt.Sprintf("https://%s:%d/", settings.Hosts[0], settings.Port)
		port = settings.Port
	}
	if info, err := os.Stat(c.p.certificate); err == nil && info.Mode().IsRegular() {
		cert = c.p.certificate
	}
	admission := defaultAuthAdmission()
	if settings != nil && settings.AuthAdmission != nil {
		admission = *settings.AuthAdmission
	}
	return map[string]any{"authAdmission": admission, "enabled": enabled, "configured": settings != nil, "running": running, "url": url, "port": port, "username": username, "certificatePath": cert, "error": errorValue}, nil
}

func readConfigurationInput(reader io.Reader) (map[string]json.RawMessage, error) {
	type result struct {
		data []byte
		err  error
	}
	ready := make(chan result, 1)
	go func() { data, err := io.ReadAll(io.LimitReader(reader, 4097)); ready <- result{data, err} }()
	select {
	case value := <-ready:
		if value.err != nil {
			return nil, errors.New("Configure expects JSON on stdin")
		}
		if len(value.data) > 4096 {
			return nil, errors.New("Configure input is too large")
		}
		fields, err := decodeObject(value.data)
		if err != nil {
			return nil, errors.New("Configure expects JSON on stdin")
		}
		if !keysAre(fields, "password") && !keysAre(fields, "password", "port") && !keysAre(fields, "password", "auth_admission") && !keysAre(fields, "password", "port", "auth_admission") {
			return nil, errors.New("Configure accepts only password, optional port and auth_admission")
		}
		return fields, nil
	case <-time.After(5 * time.Second):
		return nil, errors.New("Configure stdin did not finish before its deadline")
	}
}

type preparedConfiguration struct {
	passwordHash  string
	port          int
	authAdmission *AuthAdmissionSettings
}

func prepareConfiguration(value map[string]json.RawMessage) (*preparedConfiguration, error) {
	password, ok := text(value["password"])
	if !ok || utf8.RuneCountInString(password) < 4 || !printable(password) {
		return nil, errors.New("Use at least 4 characters. No special characters required.")
	}
	if len(password) > 72 {
		return nil, errors.New("Password must be at most 72 UTF-8 bytes for Caddy bcrypt")
	}
	port := int64(defaultPort)
	if value["port"] != nil {
		var valid bool
		port, valid = integer(value["port"], 1024, 65535)
		if !valid {
			return nil, errors.New("Port must be from 1024 to 65535")
		}
	}
	admission, err := parseAuthAdmission(value["auth_admission"])
	if err != nil {
		return nil, err
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), 14)
	if err != nil {
		return nil, err
	}
	hash := strings.TrimSpace(string(hashed))
	if !bcryptHash.MatchString(hash) {
		return nil, errors.New("Could not generate a valid password hash")
	}
	return &preparedConfiguration{passwordHash: hash, port: int(port), authAdmission: admission}, nil
}

func (c *controller) configure(value map[string]json.RawMessage) error {
	prepared, err := prepareConfiguration(value)
	if err != nil {
		return err
	}
	return c.applyConfiguration(prepared)
}

func (c *controller) applyConfiguration(prepared *preparedConfiguration) error {
	if err := c.p.prepare(); err != nil {
		return err
	}
	// Read the latest enabled choice under control.lock, after expensive hashing.
	previous, err := loadSettings(c.p)
	if err != nil {
		return err
	}
	hosts, err := c.allowedHosts()
	if err != nil {
		return err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	settings := &Settings{Enabled: previous != nil && previous.Enabled, Username: username, Port: prepared.port, Hosts: hosts, PasswordHash: prepared.passwordHash, SecretKey: hex.EncodeToString(key)}
	settings.AuthAdmission = prepared.authAdmission
	if settings.AuthAdmission == nil && previous != nil {
		settings.AuthAdmission = previous.AuthAdmission
	}
	// Caddy validation provisions modules and can generate a CA. Keep configure
	// pure with respect to gateway state; only the acknowledged service child
	// may provision the generated configuration.
	if _, err := gatewayConfiguration(c.p, settings); err != nil {
		return err
	}
	if err := writePrivate(c.p.config, settings); err != nil {
		return err
	}
	if settings.Enabled {
		return c.enable()
	}
	if _, err := os.Lstat(c.p.errorFile); err == nil || c.unresolvedChildren() {
		if err := c.stopOwnedService(); err != nil {
			return err
		}
		return removeIfExists(c.p.errorFile)
	}
	return nil
}

func xmlText(value string) string {
	var buffer bytes.Buffer
	_ = xml.EscapeText(&buffer, []byte(value))
	return buffer.String()
}
func (c *controller) launchAgent() error {
	if c.p.managedApp != "" {
		return nil // The signed app owns its service definition.
	}
	if c.p.managedCandidate != "" {
		return errors.New("Complete Agent Queue installation before enabling phone access")
	}
	if err := os.MkdirAll(filepath.Dir(c.p.plist), 0700); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	var environment strings.Builder
	for _, entry := range controlEnvironment(false) {
		parts := strings.SplitN(entry, "=", 2)
		environment.WriteString("<key>" + xmlText(parts[0]) + "</key><string>" + xmlText(parts[1]) + "</string>")
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n" +
		`<plist version="1.0"><dict><key>Label</key><string>` + label + `</string><key>ProgramArguments</key><array><string>` + xmlText(executable) + `</string><string>serve</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict><key>ThrottleInterval</key><integer>10</integer><key>ProcessType</key><string>Background</string><key>Umask</key><integer>63</integer><key>EnvironmentVariables</key><dict>` + environment.String() + `</dict><key>StandardOutPath</key><string>` + xmlText(c.p.log) + `</string><key>StandardErrorPath</key><string>` + xmlText(c.p.log) + `</string></dict></plist>`
	return writePrivate(c.p.plist, []byte(plist))
}

func (c *controller) serviceAction(action string) error {
	if c.p.managedApp != "" {
		return c.managedServiceAction(action)
	}
	if c.p.testing {
		if action != "stop" {
			return nil
		}
		record, err := c.generation()
		if err != nil || record == nil {
			return err
		}
		boot, err := c.bootFn()
		if err != nil {
			return err
		}
		if record.BootUUID != boot || record.Supervisor.PID == os.Getpid() || record.Supervisor.Birth == "" {
			return nil
		}
		birth, err := c.birthFn(record.Supervisor.PID)
		if err != nil {
			return err
		}
		if birth == record.Supervisor.Birth {
			err := c.killFn(record.Supervisor.PID, syscall.SIGTERM)
			if err != syscall.ESRCH {
				return err
			}
		}
		return nil
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	if action == "stop" {
		_, err := c.command([]string{"/bin/launchctl", "bootout", domain + "/" + label}, nil, 5*time.Second, false)
		return err
	}
	_, err := c.command([]string{"/bin/launchctl", "bootstrap", domain, c.p.plist}, nil, 5*time.Second, true)
	return err
}

func (c *controller) enable() error {
	if c.p.managedCandidate != "" && c.p.managedApp == "" {
		return errors.New("Complete Agent Queue installation before enabling phone access")
	}
	if c.p.managedApp != "" {
		if !c.installMigration {
			pending := filepath.Join(c.p.home, ".local/share/agent-heavy/install-in-progress.json")
			if _, err := appservices.ReadFile(c.p.home, pending, 16384, false); err == nil {
				return errors.New("Installation is incomplete; finish the installer before enabling phone access")
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("Cannot verify completed installation before enabling phone access: %w", err)
			}
		}
		state, err := c.managedServiceState("status")
		if err != nil {
			return err
		}
		if state == "requiresApproval" {
			return errors.New("Allow Agent Queue's background services in Login Items settings before enabling phone access")
		}
	}
	if err := c.p.prepare(); err != nil {
		return err
	}
	settings, err := loadSettings(c.p)
	if err != nil {
		return err
	}
	if settings == nil {
		return errors.New("Configure a password before enabling remote access")
	}
	hosts, err := c.allowedHosts()
	if err != nil {
		return err
	}
	settings.Enabled = true
	settings.Hosts = hosts
	if err := writePrivate(c.p.config, settings); err != nil {
		return err
	}
	if err := c.stopOwnedService(); err != nil {
		return err
	}
	if err := c.launchAgent(); err != nil {
		return err
	}
	if err := c.serviceFn("start"); err != nil {
		return err
	}
	if !c.p.testing {
		deadline := time.Now().Add(15 * time.Second)
		for !c.isRunning() && time.Now().Before(deadline) && c.ctx.Err() == nil {
			time.Sleep(150 * time.Millisecond)
		}
		if !c.isRunning() {
			return errors.New("Remote access was enabled but its service did not become ready")
		}
	}
	return removeIfExists(c.p.errorFile)
}

func (c *controller) disable() error {
	if err := c.p.prepare(); err != nil {
		return err
	}
	settings, err := loadSettings(c.p)
	if err != nil {
		return err
	}
	if settings != nil {
		settings.Enabled = false
		if err := writePrivate(c.p.config, settings); err != nil {
			return err
		}
	}
	if err := c.stopOwnedService(); err != nil {
		return err
	}
	return removeIfExists(c.p.errorFile)
}

// migrate is invoked by the installer while Main holds control.lock. Read the
// latest user choice after acquiring that lock; a stale installation snapshot
// must never turn access back on after the user disabled it.
func (c *controller) migrate() error {
	settings, err := loadSettings(c.p)
	if err != nil {
		return err
	}
	if settings != nil && settings.Enabled {
		return c.enable()
	}
	return c.disable()
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
func (c *controller) deactivate(token string) error {
	unlock, err := c.lock("registry.lock", false)
	if err != nil {
		return err
	}
	defer unlock()
	record, err := c.generation()
	if err != nil {
		return err
	}
	if record != nil && (token == "" || record.Token == token) {
		record.Active = false
		record.Ready = false
		return writePrivate(c.p.pid, record)
	}
	return nil
}

func (c *controller) childRecords() ([]string, error) {
	return filepath.Glob(filepath.Join(c.p.children, "*.json"))
}
func (c *controller) validateChild(path string, record childRecord) error {
	expected, err := c.p.childPath(record.Generation, record.Role)
	if err != nil || path != expected || record.PID <= 1 || record.Birth == "" || record.BootUUID == "" {
		return errors.New("Unknown service child record; refusing to signal it")
	}
	return nil
}

func (c *controller) reconcileChildren(token string) error {
	boot, err := c.bootFn()
	if err != nil {
		return err
	}
	paths, err := c.childRecords()
	if err != nil {
		return err
	}
	for _, path := range paths {
		var record childRecord
		if err := readPrivate(path, &record); err != nil {
			return err
		}
		if token != "" && record.Generation != token {
			continue
		}
		if err := c.validateChild(path, record); err != nil {
			return err
		}
		birth, err := c.birthFn(record.PID)
		if err != nil {
			return err
		}
		if record.BootUUID == boot && birth == record.Birth {
			for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
				birth, err = c.birthFn(record.PID)
				if err != nil {
					return err
				}
				if birth != record.Birth {
					break
				}
				if record.PID == os.Getpid() {
					return errors.New("Owned service group identity could not be verified")
				}
				group, err := c.groupFn(record.PID)
				if err == syscall.ESRCH {
					break
				}
				if err != nil {
					return err
				}
				if group != record.PID {
					return errors.New("Owned service group identity could not be verified")
				}
				// Recheck after group lookup: a recycled PID must never receive a signal.
				birth, err = c.birthFn(record.PID)
				if err != nil {
					return err
				}
				if birth != record.Birth {
					break
				}
				if err := c.killGroupFn(record.PID, signal); err == syscall.ESRCH {
					break
				} else if err != nil {
					return err
				}
				deadline := time.Now().Add(2 * time.Second)
				for {
					birth, err = c.birthFn(record.PID)
					if err != nil {
						return err
					}
					if birth != record.Birth || time.Now().After(deadline) {
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
			birth, err = c.birthFn(record.PID)
			if err != nil {
				return err
			}
			if birth == record.Birth {
				return errors.New("An owned remote service process did not stop")
			}
		}
		if err := removeIfExists(path); err != nil {
			return err
		}
	}
	return nil
}

func (c *controller) unresolvedChildren() bool {
	paths, err := c.childRecords()
	if err != nil {
		return true
	}
	if len(paths) == 0 {
		return false
	}
	boot, err := c.bootFn()
	if err != nil {
		return true
	}
	for _, path := range paths {
		var record childRecord
		if readPrivate(path, &record) != nil || c.validateChild(path, record) != nil {
			return true
		}
		birth, err := c.birthFn(record.PID)
		if err != nil {
			return true
		}
		if record.BootUUID == boot && birth == record.Birth {
			return true
		}
	}
	return false
}

func (c *controller) stopSupervisor() error {
	return c.stopSupervisorWithGrace(3 * time.Second)
}

func (c *controller) stopSupervisorWithGrace(grace time.Duration) error {
	record, err := c.generation()
	if err != nil || record == nil {
		return err
	}
	boot, err := c.bootFn()
	if err != nil {
		return err
	}
	if record.BootUUID != boot || record.Supervisor.Birth == "" {
		return nil
	}
	pid := record.Supervisor.PID
	if pid <= 1 {
		return errors.New("Invalid supervisor identity; refusing to signal it")
	}
	birth, err := c.birthFn(pid)
	if err != nil {
		return err
	}
	if birth != record.Supervisor.Birth {
		return nil
	}
	if pid == os.Getpid() {
		return errors.New("Refusing to stop the current control process")
	}
	if err := c.killFn(pid, syscall.SIGTERM); err == syscall.ESRCH {
		return nil
	} else if err != nil {
		return err
	}
	deadline := time.Now().Add(grace)
	for {
		birth, err = c.birthFn(pid)
		if err != nil {
			return err
		}
		if birth != record.Supervisor.Birth {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// A PID can be recycled after the final wait-loop observation. Establish the
	// complete identity again immediately before escalation.
	boot, err = c.bootFn()
	if err != nil {
		return err
	}
	if boot != record.BootUUID {
		return nil
	}
	birth, err = c.birthFn(pid)
	if err != nil {
		return err
	}
	if birth != record.Supervisor.Birth {
		return nil
	}
	if err := c.killFn(pid, syscall.SIGKILL); err != syscall.ESRCH {
		return err
	}
	return nil
}

func (c *controller) stopOwnedService() error {
	var failures []string
	for _, action := range []func() error{func() error { return c.deactivate("") }, func() error { return c.serviceFn("stop") }, c.stopSupervisor} {
		if err := action(); err != nil {
			failures = append(failures, err.Error())
		}
	}
	unlock, err := c.lock("registry.lock", false)
	if err != nil {
		failures = append(failures, err.Error())
	} else {
		err = c.reconcileFn("")
		if err == nil {
			var record *generationRecord
			record, err = c.generation()
			if err == nil && record != nil && !record.Active {
				err = removeIfExists(c.p.pid)
			}
		}
		unlock()
		if err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		message := "Remote shutdown reported errors: " + strings.Join(failures, "; ")
		if err := writePrivate(c.p.errorFile, map[string]any{"error": message}); err != nil {
			return fmt.Errorf("%s; failure state could not be saved: %w", message, err)
		}
		return errors.New(message)
	}
	return nil
}

func (c *controller) parentValid(record *generationRecord, settings *Settings, token string) bool {
	if record == nil || settings == nil || record.Token != token || !record.Active || !settings.Enabled || record.Revision != revision(settings) || record.Supervisor.Birth == "" {
		return false
	}
	boot, err := c.bootFn()
	if err != nil || boot != record.BootUUID {
		return false
	}
	birth, err := c.birthFn(record.Supervisor.PID)
	return err == nil && birth == record.Supervisor.Birth
}

func (c *controller) childMain(role, token string) error {
	if role != "api" {
		return errors.New("Legacy gateway children cannot be started")
	}
	path, err := c.p.childPath(token, role)
	if err != nil {
		return err
	}
	own, err := c.record(os.Getpid())
	if err != nil {
		return err
	}
	boot, err := c.bootFn()
	if err != nil {
		return err
	}
	unlock, err := c.lock("registry.lock", false)
	if err != nil {
		return err
	}
	parent, err := c.generation()
	if err != nil {
		unlock()
		return err
	}
	settings, err := loadSettings(c.p)
	if err != nil {
		unlock()
		return err
	}
	if !c.parentValid(parent, settings, token) {
		unlock()
		return nil
	}
	if err := writePrivate(path, childRecord{PID: own.PID, Birth: own.Birth, Generation: token, Role: role, BootUUID: boot}); err != nil {
		unlock()
		return err
	}
	unlock()
	acknowledged := false
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		parent, err := c.generation()
		if err != nil {
			return err
		}
		settings, err := loadSettings(c.p)
		if err != nil {
			return err
		}
		if !c.parentValid(parent, settings, token) {
			return nil
		}
		var record childRecord
		if err := readPrivate(path, &record); err != nil {
			return err
		}
		if record.Ack {
			acknowledged = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !acknowledged {
		return nil
	}
	return c.apiProcess(token)
}

func (c *controller) acknowledgedAPI(token string) (*Settings, error) {
	parent, err := c.generation()
	if err != nil {
		return nil, err
	}
	settings, err := loadSettings(c.p)
	if err != nil {
		return nil, err
	}
	if !c.parentValid(parent, settings, token) || parent.Gateway != embeddedGateway {
		return nil, errors.New("Remote service generation is no longer active")
	}
	path, err := c.p.childPath(token, "api")
	if err != nil {
		return nil, err
	}
	var child childRecord
	if err := readPrivate(path, &child); err != nil {
		return nil, err
	}
	own, err := c.record(os.Getpid())
	if err != nil {
		return nil, err
	}
	if !child.Ack || child.PID != own.PID || child.Birth != own.Birth || child.Generation != token || child.Role != "api" || child.BootUUID != parent.BootUUID {
		return nil, errors.New("Embedded gateway requires an acknowledged owned service process")
	}
	return settings, nil
}

func (c *controller) apiProcess(token string) error {
	settings, err := c.acknowledgedAPI(token)
	if err != nil {
		return err
	}
	if err := validateCAStorage(c.p); err != nil {
		return err
	}
	if err := prepareGatewayLog(c.p); err != nil {
		return err
	}
	config, err := gatewayConfiguration(c.p, settings)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(c.ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	var runner Runner
	if c.p.testing && os.Getenv("AGENT_REMOTE_TEST_CONTROLLER") != "" {
		executable := os.Getenv("AGENT_REMOTE_TEST_CONTROLLER")
		if !filepath.IsAbs(executable) {
			return errors.New("Test controller must be an absolute executable")
		}
		runner = executableRunner(executable)
	}
	handler, err := NewHandler(*settings, runner, APIOptions{TestMode: c.p.testing})
	if err != nil {
		return err
	}
	listener, err := net.Listen("unix", c.p.socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(c.p.socket, 0600); err != nil {
		return err
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Only Caddy can reach the user-private socket. Ignore all client proxy
			// metadata except the HTTPS marker that Caddy overwrites itself.
			if r.Header.Get("X-Forwarded-Proto") == "https" {
				r.URL.Scheme = "https"
			}
			r.Header.Del("Authorization")
			r.Header.Del("Forwarded")
			r.Header.Del("X-Forwarded-Host")
			handler.ServeHTTP(w, r)
		})}
	// Bound concurrent accepted connections without another runtime dependency.
	limited := &limitedListener{Listener: listener, slots: make(chan struct{}, 32)}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(limited) }()
	defer server.Close()
	if _, err := c.acknowledgedAPI(token); err != nil {
		return err
	}
	if err := caddy.Load(config, true); err != nil {
		return errors.New("Embedded HTTPS gateway could not start; inspect the private gateway log")
	}
	defer caddy.Stop()
	deadline := time.Now().Add(10 * time.Second)
	for !gatewayReady(ctx, c.p, settings) {
		if ctx.Err() != nil {
			return nil
		}
		select {
		case err := <-serverDone:
			return err
		default:
		}
		if time.Now().After(deadline) {
			return errors.New("Embedded HTTPS gateway did not become ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
	unlock, err := c.lock("registry.lock", false)
	if err != nil {
		return err
	}
	if _, err := c.acknowledgedAPI(token); err != nil {
		unlock()
		return err
	}
	path, _ := c.p.childPath(token, "api")
	var child childRecord
	if err := readPrivate(path, &child); err != nil {
		unlock()
		return err
	}
	child.Ready = true
	err = writePrivate(path, child)
	unlock()
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-serverDone:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

type limitedListener struct {
	net.Listener
	slots chan struct{}
}
type limitedConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (l *limitedListener) Accept() (net.Conn, error) {
	l.slots <- struct{}{}
	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &limitedConn{Conn: conn, release: func() { <-l.slots }}, nil
}
func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

type serviceChild struct {
	cmd  *exec.Cmd
	done chan error
}

func (child *serviceChild) departed() bool {
	select {
	case <-child.done:
		return true
	default:
		return false
	}
}

func selfArguments(action ...string) ([]string, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := []string{executable}
	if os.Getenv("AGENT_REMOTE_TEST_MODE") == "1" && os.Getenv("AGENT_REMOTE_TEST_HELPER") == "1" {
		args = append(args, "-test.run=^TestRemoteProcessHelper$", "--")
	}
	return append(args, action...), nil
}

func (c *controller) supervisor() (result error) {
	if err := c.p.prepare(); err != nil {
		return err
	}
	settings, err := loadSettings(c.p)
	if err != nil {
		return err
	}
	if settings == nil || !settings.Enabled {
		return nil
	}
	unlockSupervisor, err := c.lock("supervisor.lock", true)
	if err != nil {
		return errors.New("Remote supervisor is already running")
	}
	defer unlockSupervisor()
	ctx, stop := signal.NotifyContext(c.ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	if err := c.deactivate(""); err != nil {
		return err
	}
	unlock, err := c.lock("registry.lock", false)
	if err != nil {
		return err
	}
	if err := c.reconcileFn(""); err != nil {
		unlock()
		return err
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		unlock()
		return err
	}
	token := hex.EncodeToString(key)
	boot, err := c.bootFn()
	if err != nil {
		unlock()
		return err
	}
	own, err := c.record(os.Getpid())
	if err != nil {
		unlock()
		return err
	}
	if err := writePrivate(c.p.pid, generationRecord{Token: token, Active: true, BootUUID: boot, Revision: revision(settings), Supervisor: own, Gateway: embeddedGateway}); err != nil {
		unlock()
		return err
	}
	unlock()
	var children []*serviceChild
	defer func() {
		var cleanup []error
		if err := c.deactivate(token); err != nil {
			cleanup = append(cleanup, err)
		}
		unlock, err := c.lock("registry.lock", false)
		if err != nil {
			cleanup = append(cleanup, err)
		} else {
			if err := c.reconcileFn(token); err != nil {
				cleanup = append(cleanup, err)
			} else {
				current, err := c.generation()
				if err != nil {
					cleanup = append(cleanup, err)
				} else if current != nil && current.Token == token {
					if err := removeIfExists(c.p.pid); err != nil {
						cleanup = append(cleanup, err)
					}
				}
			}
			unlock()
		}
		for _, child := range children {
			select {
			case <-child.done:
			case <-time.After(3 * time.Second):
				cleanup = append(cleanup, errors.New("Owned remote child did not exit"))
			}
		}
		if len(cleanup) == 0 {
			if info, err := os.Lstat(c.p.socket); err == nil && info.Mode()&os.ModeSocket != 0 && ownedByCurrentUser(info) {
				_ = os.Remove(c.p.socket)
			}
		}
		result = errors.Join(append([]error{result}, cleanup...)...)
		if result != nil {
			_ = writePrivate(c.p.errorFile, map[string]any{"error": result.Error()})
		}
	}()
	if info, err := os.Lstat(c.p.socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 || !ownedByCurrentUser(info) {
			return errors.New("Refusing to remove an unknown API socket")
		}
		if err := os.Remove(c.p.socket); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	configuration, err := gatewayConfiguration(c.p, settings)
	if err != nil {
		return err
	}
	if err := writePrivate(c.p.gatewayConfig, configuration); err != nil {
		return err
	}
	fd, err := syscall.Open(c.p.log, syscall.O_CREAT|syscall.O_APPEND|syscall.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	log := os.NewFile(uintptr(fd), c.p.log)
	defer log.Close()
	info, err := log.Stat()
	if err != nil || !info.Mode().IsRegular() || !ownedByCurrentUser(info) || info.Mode().Perm()&0077 != 0 {
		return errors.New("Unsafe remote service log")
	}
	spawn := func(role string) (*serviceChild, error) {
		args, err := selfArguments("child", role, token)
		if err != nil {
			return nil, err
		}
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = controlEnvironment(c.p.testing)
		cmd.Stderr = log
		cmd.Stdout = log
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		child := &serviceChild{cmd: cmd, done: make(chan error)}
		children = append(children, child)
		go func() { _ = cmd.Wait(); close(child.done) }()
		path, err := c.p.childPath(token, role)
		if err != nil {
			return nil, err
		}
		deadline := time.Now().Add(8 * time.Second)
		for {
			_, err := os.Lstat(path)
			if err == nil || child.departed() || time.Now().After(deadline) || ctx.Err() != nil {
				break
			}
			time.Sleep(30 * time.Millisecond)
		}
		if c.p.testing && role == "api" {
			gate := os.Getenv("AGENT_REMOTE_TEST_ACK_GATE")
			for gate != "" && time.Now().Before(deadline) && ctx.Err() == nil {
				if _, err := os.Stat(gate); err == nil {
					break
				}
				time.Sleep(30 * time.Millisecond)
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		unlock, err := c.lock("registry.lock", false)
		if err != nil {
			return nil, err
		}
		defer unlock()
		var record childRecord
		if err := readPrivate(path, &record); err != nil {
			return nil, errors.New("Service child registration was not acknowledged")
		}
		current, err := c.generation()
		if err != nil {
			return nil, err
		}
		birth, err := c.birthFn(cmd.Process.Pid)
		if err != nil {
			return nil, err
		}
		if current == nil || current.Token != token || !current.Active || record.PID != cmd.Process.Pid || record.Birth == "" || birth != record.Birth || record.BootUUID != boot || record.Generation != token || record.Role != role {
			return nil, errors.New("Service child registration was not acknowledged")
		}
		record.Ack = true
		if err := writePrivate(path, record); err != nil {
			return nil, err
		}
		return child, nil
	}
	api, err := spawn("api")
	if err != nil {
		return err
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		info, err := os.Lstat(c.p.socket)
		if err == nil && info.Mode()&os.ModeSocket != 0 {
			break
		}
		if api.departed() || time.Now().After(deadline) || ctx.Err() != nil {
			return errors.New("Private queue API did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	deadline = time.Now().Add(10 * time.Second)
	ready := false
	for time.Now().Before(deadline) && !api.departed() && ctx.Err() == nil {
		path, _ := c.p.childPath(token, "api")
		var registered childRecord
		if readPrivate(path, &registered) == nil && registered.Ready && gatewayReady(ctx, c.p, settings) {
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		return errors.New("HTTPS gateway did not start")
	}
	unlock, err = c.lock("registry.lock", false)
	if err != nil {
		return err
	}
	current, err := c.generation()
	if err != nil {
		unlock()
		return err
	}
	if current == nil || current.Token != token || !current.Active {
		unlock()
		return errors.New("Service generation was superseded during startup")
	}
	current.Ready = true
	err = writePrivate(c.p.pid, current)
	unlock()
	if err != nil {
		return err
	}
	if err := removeIfExists(c.p.errorFile); err != nil {
		return err
	}
	for ctx.Err() == nil {
		current, err := c.generation()
		if err != nil {
			return err
		}
		if current == nil || current.Token != token || !current.Active {
			break
		}
		if slices.ContainsFunc(children, func(child *serviceChild) bool { return child.departed() }) {
			return errors.New("A remote service child exited unexpectedly")
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

// Main is the standalone remote controller entrypoint.
func Main(arguments []string) int {
	return runMain(arguments, os.Stdin, prepareConfiguration, defaultLifecycleContext)
}

func defaultLifecycleContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), appservices.ControllerLifecycleTimeout)
}

func runMain(arguments []string, inputReader io.Reader, prepare func(map[string]json.RawMessage) (*preparedConfiguration, error), lifecycleContext func() (context.Context, context.CancelFunc)) int {
	if len(arguments) == 1 && (arguments[0] == "--help" || arguments[0] == "-h") {
		fmt.Println(remoteUsage)
		return 0
	}
	if len(arguments) == 2 && (arguments[1] == "--help" || arguments[1] == "-h") {
		switch arguments[0] {
		case "status", "configure", "enable", "disable", "serve":
			fmt.Println(remoteUsage)
			return 0
		}
	}
	previousUmask := syscall.Umask(0077)
	defer syscall.Umask(previousUmask)
	if len(arguments) == 0 {
		return printFailure(errors.New(remoteUsage))
	}
	action := arguments[0]
	valid := false
	switch action {
	case "status":
		valid = len(arguments) == 1 || len(arguments) == 2 && arguments[1] == "--json"
	case "configure", "enable", "disable", "migrate", "serve":
		valid = len(arguments) == 1
	case "child":
		valid = len(arguments) == 3 && arguments[1] == "api" && tokenPattern.MatchString(arguments[2])
	}
	if !valid {
		return printFailure(errors.New("Invalid remote controller arguments"))
	}
	p, err := newPaths()
	if err != nil {
		return printFailure(err)
	}
	var prepared *preparedConfiguration
	var preparationErr error
	if action == "configure" {
		var input map[string]json.RawMessage
		input, preparationErr = readConfigurationInput(inputReader)
		if preparationErr == nil {
			prepared, preparationErr = prepare(input)
		}
	}
	// Credential preparation has no service side effects and must not consume
	// the finite stop/start budget, especially on slower CPUs. Only lifecycle
	// work holds control.lock; applyConfiguration rereads authoritative settings.
	ctx := context.Background()
	var cancel context.CancelFunc
	if action == "configure" || action == "enable" || action == "disable" || action == "migrate" {
		ctx, cancel = lifecycleContext()
		defer cancel()
	}
	c := newController(p, ctx)
	c.installMigration = action == "migrate"
	switch action {
	case "configure", "enable", "disable", "migrate":
		if err = p.prepare(); err != nil {
			return printFailure(err)
		}
		unlock, lockErr := c.lock("control.lock", false)
		if lockErr != nil {
			return printFailure(lockErr)
		}
		defer unlock()
		switch action {
		case "configure":
			err = preparationErr
			if err == nil {
				err = c.applyConfiguration(prepared)
			}
		case "enable":
			err = c.enable()
		case "disable":
			err = c.disable()
		case "migrate":
			err = c.migrate()
		}
		if err != nil {
			if saveErr := writePrivate(p.errorFile, map[string]any{"error": err.Error()}); saveErr != nil {
				return printFailure(errors.New("Remote control failed and its failure state could not be saved"))
			}
			return printFailure(err)
		}
	case "serve":
		err = c.supervisor()
		if err != nil {
			return printFailure(err)
		}
		return 0
	case "child":
		err = c.childMain(arguments[1], arguments[2])
		if err != nil {
			return printFailure(err)
		}
		return 0
	}
	status, err := c.status()
	if err != nil {
		return printFailure(err)
	}
	_ = json.NewEncoder(os.Stdout).Encode(status)
	return 0
}

func printFailure(err error) int {
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"error": err.Error()})
	return 125
}
