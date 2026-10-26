// Package backend implements the private, daemon-backed command queue.
package backend

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Object retains the versioned JSON shape shared with existing installations.
// Values crossing a filesystem, process, or HTTP interface are validated by the
// owning operation before they can authorize work or mutate private state.
type Object = map[string]any

const ToolPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
const MaxParallel = 8

var overrides = []string{"AGENT_HEAVY_CONFIG", "AGENT_HEAVY_STATE", "AGENT_HEAVY_LEGACY_SLOT", "AGENT_HEAVY_PUEUE"}

func obj(value any) Object { result, _ := value.(map[string]any); return result }
func list(value any) []any {
	switch v := value.(type) {
	case []any:
		return v
	case []Object:
		result := make([]any, len(v))
		for i := range v {
			result[i] = v[i]
		}
		return result
	case []string:
		result := make([]any, len(v))
		for i := range v {
			result[i] = v[i]
		}
		return result
	}
	return nil
}
func text(value any) string  { result, _ := value.(string); return result }
func boolean(value any) bool { result, _ := value.(bool); return result }
func integer(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		n := int(v)
		return n, int64(n) == v
	case uint64:
		if v <= math.MaxInt {
			return int(v), true
		}
	case json.Number:
		n, err := strconv.ParseInt(string(v), 10, 64)
		return int(n), err == nil && int64(int(n)) == n
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) && v >= math.MinInt64 && v < float64(math.MaxInt64) && math.Trunc(v) == v {
			return int(v), true
		}
	}
	return 0, false
}
func number(value any) (float64, bool) {
	var n float64
	switch v := value.(type) {
	case float64:
		n = v
	case float32:
		n = float64(v)
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	case uint64:
		n = float64(v)
	case json.Number:
		var err error
		n, err = v.Float64()
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	return n, !math.IsNaN(n) && !math.IsInf(n, 0)
}
func token() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func validToken(value string) bool {
	if len(value) != 32 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}
func envMap() map[string]string {
	result := map[string]string{}
	for _, item := range os.Environ() {
		k, v, ok := strings.Cut(item, "=")
		if ok {
			result[k] = v
		}
	}
	return result
}
func envList(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for k, v := range values {
		result = append(result, k+"="+v)
	}
	return result
}
func envDefault(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}
func stringsFrom(value any) ([]string, error) {
	values := list(value)
	if values == nil {
		return nil, errors.New("Expected an argument list")
	}
	result := make([]string, len(values))
	for i, v := range values {
		s, ok := v.(string)
		if !ok || strings.ContainsRune(s, 0) {
			return nil, errors.New("Invalid command arguments")
		}
		result[i] = s
	}
	return result, nil
}

func decodeObject(data []byte) (Object, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value Object
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errors.New("Expected a JSON object")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("Trailing JSON content")
	}
	return value, nil
}

func fileOwned(info os.FileInfo, private bool) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() || !info.Mode().IsRegular() {
		return false
	}
	mask := os.FileMode(0o022)
	if private {
		mask = 0o077
	}
	return info.Mode().Perm()&mask == 0
}

// ReadPrivate refuses symlinks and files readable or writable by other users.
func ReadPrivate(path string) (Object, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !fileOwned(info, true) {
		return nil, fmt.Errorf("Unsafe private job file: %s", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, 32*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 32*1024*1024 {
		return nil, errors.New("Private job file exceeds the size limit")
	}
	return decodeObject(data)
}

func WritePrivate(path string, value Object) error {
	if info, err := os.Lstat(path); err == nil {
		if !fileOwned(info, true) {
			return fmt.Errorf("Unsafe private job file: %s", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+"."+token()+".tmp")
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(temporary)
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temporary, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func privateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("Unsafe private directory: %s", path)
	}
	return os.Chmod(path, 0o700)
}

type Gate struct {
	Config, State, Slot, Jobs, Receipts, Cancelled, Metadata string
	ControlEnv                                               map[string]string
	ControlDeadline                                          time.Time
	controlContext                                           context.Context
	queueControl                                             func(string, Object) (Object, error)
	DescriptionCache, ProjectCache                           map[string]Object
}

func NewGate() (*Gate, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	state := envDefault("AGENT_HEAVY_STATE", filepath.Join(home, ".local/state/agent-heavy"))
	g := &Gate{Config: envDefault("AGENT_HEAVY_CONFIG", filepath.Join(home, ".config/agent-heavy/pueue.yml")), State: state, ControlEnv: map[string]string{}, DescriptionCache: map[string]Object{}, ProjectCache: map[string]Object{}}
	g.Jobs = filepath.Join(state, "jobs")
	g.Receipts = filepath.Join(state, "receipts")
	g.Cancelled = filepath.Join(state, "cancelled")
	g.Metadata = filepath.Join(state, "metadata")
	names := append([]string{"HOME", "PATH", "USER", "LOGNAME", "TMPDIR", "LANG", "LC_ALL"}, overrides...)
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			g.ControlEnv[name] = value
		}
	}
	if _, ok := g.ControlEnv["PATH"]; !ok {
		g.ControlEnv["PATH"] = ToolPath
	}
	g.Slot, err = LeaseSlot(state)
	return g, err
}
func LeaseSlot(state string) (string, error) {
	value, present := os.LookupEnv("AGENT_HEAVY_LEGACY_SLOT")
	if !present {
		settings, err := ReadPrivate(filepath.Join(state, "legacy-slot.json"))
		if errors.Is(err, os.ErrNotExist) {
			return filepath.Join(state, "execution.slot"), nil
		}
		if err != nil {
			return "", fmt.Errorf("Cannot safely read the retained legacy lease configuration: %w", err)
		}
		if len(settings) != 1 {
			return "", errors.New("Invalid retained legacy lease configuration fields")
		}
		value = text(settings["slot"])
	}
	if value == "" || strings.ContainsRune(value, 0) || !filepath.IsAbs(value) {
		return "", errors.New("Legacy lease configuration must name an absolute path")
	}
	return value, nil
}
func (g *Gate) Prepare() error {
	for _, path := range []string{g.State, g.Jobs, g.Receipts, g.Cancelled, g.Metadata, filepath.Join(g.State, "workloads"), filepath.Join(g.State, "resource-runs"), filepath.Join(g.State, "run"), filepath.Join(g.State, "logs")} {
		if err := privateDirectory(path); err != nil {
			return err
		}
	}
	return nil
}
func (g *Gate) ReceiptPath(id int) string { return filepath.Join(g.Receipts, strconv.Itoa(id)+".json") }
func (g *Gate) PayloadPath(value string) (string, error) {
	if !validToken(value) {
		return "", errors.New("Invalid private job token")
	}
	return filepath.Join(g.Jobs, value+".json"), nil
}
func (g *Gate) Receipt(id int) (Object, error) {
	value, err := ReadPrivate(g.ReceiptPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return value, err
}

type boundedOutput struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedOutput) ReadFrom(reader io.Reader) (int64, error) {
	// bytes.Buffer's promoted ReaderFrom would let io.Copy bypass Write and
	// its capture limit. Keep the destination's only fast path bounded too.
	return io.Copy(struct{ io.Writer }{b}, reader)
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		b.exceeded = true
		return 0, errors.New("Control output exceeds size limit")
	}
	return b.Buffer.Write(p)
}

// Snapshot retains the private task shape while callers migrate to typed jobs.
func (g *Gate) Snapshot() (Object, error) {
	return g.QueueControl("snapshot", nil)
}

func (g *Gate) operationContext() context.Context {
	if g.controlContext != nil {
		return g.controlContext
	}
	return context.Background()
}
func (g *Gate) Task(id int) (Object, error) {
	snapshot, err := g.Snapshot()
	if err != nil {
		return nil, err
	}
	raw := obj(snapshot["tasks"])[strconv.Itoa(id)]
	if raw == nil {
		return nil, nil
	}
	value := obj(raw)
	if value == nil {
		return nil, errors.New("Queue returned an invalid task")
	}
	return value, nil
}

func LockPrivate(g *Gate, name string) (func(), error) {
	ctx := g.operationContext()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if name == "" {
		name = "lease.lock"
	}
	if filepath.Base(name) != name {
		return nil, errors.New("Invalid coordination lock name")
	}
	path := filepath.Join(g.State, name)
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !fileOwned(info, true) || stat.Nlink != 1 {
		file.Close()
		return nil, errors.New("Unsafe queue coordination lock")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			file.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			file.Close()
			return nil, errors.New("Queue coordination lock did not become available")
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = file.Close() }, nil
}

func TaskStatus(task Object) (string, Object, error) {
	if task == nil {
		return "Missing", Object{}, nil
	}
	status := obj(task["status"])
	if len(status) != 1 {
		return "", nil, errors.New("Queue returned invalid task status")
	}
	for name, raw := range status {
		details := obj(raw)
		if details == nil {
			return "", nil, errors.New("Queue returned invalid task status details")
		}
		return name, details, nil
	}
	return "", nil, errors.New("Invalid task status")
}
func ResultCode(result any) (int, error) {
	if result == "Success" {
		return 0, nil
	}
	if result == "Killed" {
		return 137, nil
	}
	if result == "Errored" || result == "DependencyFailed" {
		return 125, nil
	}
	if value := obj(result); value != nil {
		if len(value) == 1 {
			if code, ok := integer(value["Failed"]); ok && code > 0 && code <= 255 {
				return code, nil
			}
		}
		if _, ok := value["FailedToSpawn"]; ok {
			return 125, nil
		}
	}
	return 0, errors.New("Queue returned an unknown task result")
}
