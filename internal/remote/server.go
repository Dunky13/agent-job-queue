// Package remote implements the opt-in private HTTPS dashboard. Caddy owns
// authentication and TLS; the queue API listens exclusively on a private socket.
package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	assets "github.com/Dunky13/agent-job-queue/remote/static"
	"github.com/gorilla/csrf"
)

const maxLog = 65536

var profileID = regexp.MustCompile(`^[0-9a-f]{64}$`)
var pageOffset = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})$`)
var pageLimit = regexp.MustCompile(`^[1-9][0-9]{0,2}$`)

// Runner invokes the allowlisted local controller. Mutation payloads use stdin.
type Runner func(context.Context, []string, []byte) (map[string]any, error)

// APIOptions permits a plaintext seam only for an explicitly injected fixture.
type APIOptions struct {
	TestMode       bool
	Scheme         string
	InsecureCookie bool
	Static         fs.FS
	// FixtureHandler is available only with a test runner and explicit test mode.
	FixtureHandler http.Handler
}

// NewHandler preserves the dashboard API without exposing command execution.
func NewHandler(settings Settings, runner Runner, options APIOptions) (http.Handler, error) {
	scheme := options.Scheme
	if scheme == "" {
		scheme = "https"
	}
	if scheme != "https" && scheme != "http" {
		return nil, errors.New("Invalid remote scheme")
	}
	if (scheme != "https" || options.InsecureCookie) && !(options.TestMode && runner != nil) {
		return nil, errors.New("Production remote access requires HTTPS and secure cookies")
	}
	if options.FixtureHandler != nil && !(options.TestMode && runner != nil) {
		return nil, errors.New("Fixture endpoints require an injected test runner")
	}
	if len(settings.Hosts) == 0 || settings.Port < 1 || settings.Port > 65535 || len(settings.SecretKey) < 32 {
		return nil, errors.New("Invalid remote API settings")
	}
	if runner == nil {
		runner = coreRunner
	}
	static := options.Static
	if static == nil {
		static = assets.Files
	}
	api := &queueAPI{run: runner, testing: options.TestMode, static: static, fixture: options.FixtureHandler}
	key := sha256.Sum256([]byte(settings.SecretKey))
	protected := csrf.Protect(key[:], csrf.CookieName("agent_remote_csrf"), csrf.Path("/"),
		csrf.Secure(!options.InsecureCookie), csrf.HttpOnly(true), csrf.SameSite(csrf.SameSiteStrictMode),
		csrf.MaxAge(3600), csrf.RequestHeader("X-CSRFToken"),
		csrf.ErrorHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			jsonError(w, 400, "CSRF validation failed; refresh this page")
		})))(api)
	authorities := make(map[string]bool)
	origins := make(map[string]bool)
	for _, host := range settings.Hosts {
		authority := strings.ToLower(host) + ":" + strconv.Itoa(settings.Port)
		authorities[authority] = true
		origins[scheme+"://"+authority] = true
		if settings.Port == 443 && scheme == "https" || settings.Port == 80 && scheme == "http" {
			authorities[strings.ToLower(host)] = true
			origins[scheme+"://"+strings.ToLower(host)] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		actualScheme := r.URL.Scheme
		if actualScheme == "" {
			if r.TLS != nil {
				actualScheme = "https"
			} else {
				actualScheme = "http"
			}
		}
		if !authorities[strings.ToLower(r.Host)] || actualScheme != scheme {
			jsonError(w, 400, "Unrecognized request origin")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if !origins[r.Header.Get("Origin")] {
				jsonError(w, 403, "Cross-origin mutation rejected")
				return
			}
			// Keep strict HTTPS referrer validation even when Origin is supplied.
			if scheme == "https" {
				ref, err := url.Parse(r.Referer())
				if err != nil || ref.User != nil || ref.Scheme != scheme || !strings.EqualFold(ref.Host, r.Host) {
					jsonError(w, 400, "CSRF validation failed; refresh this page")
					return
				}
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if scheme == "http" {
			r = csrf.PlaintextHTTPRequest(r)
		}
		protected.ServeHTTP(w, r)
	}), nil
}

type queueAPI struct {
	run     Runner
	testing bool
	static  fs.FS
	fixture http.Handler
}

func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func jsonError(w http.ResponseWriter, status int, message string) {
	jsonResponse(w, status, map[string]any{"error": message})
}

func (api *queueAPI) invoke(w http.ResponseWriter, r *http.Request, arguments []string, input []byte) {
	value, err := api.run(r.Context(), arguments, input)
	if err != nil {
		jsonError(w, 502, err.Error())
		return
	}
	jsonResponse(w, 200, value)
}

func (api *queueAPI) mutate(w http.ResponseWriter, r *http.Request, arguments []string) {
	if _, err := api.run(r.Context(), arguments, nil); err != nil {
		jsonError(w, 502, err.Error())
		return
	}
	jsonResponse(w, 200, map[string]any{"ok": true})
}

func readObject(w http.ResponseWriter, r *http.Request, optional bool) (map[string]json.RawMessage, bool) {
	if contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); !optional && contentType != "application/json" {
		jsonError(w, 415, "Unsupported Media Type")
		return nil, false
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			jsonError(w, 413, "Request Entity Too Large")
		} else {
			jsonError(w, 400, "Bad Request")
		}
		return nil, false
	}
	if optional && (len(bytes.TrimSpace(data)) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null"))) {
		return map[string]json.RawMessage{}, true
	}
	value, err := decodeObject(data)
	if err != nil {
		jsonError(w, 400, "Bad Request")
		return nil, false
	}
	return value, true
}

// decodeObject rejects duplicate keys as well as trailing JSON and non-objects.
func decodeObject(data []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("Invalid UTF-8 JSON")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return nil, errors.New("Expected JSON object")
	}
	value := make(map[string]json.RawMessage)
	for d.More() {
		keyToken, err := d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("Invalid JSON key")
		}
		if _, exists := value[key]; exists {
			return nil, errors.New("Duplicate JSON field")
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return nil, err
		}
		value[key] = raw
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("Trailing JSON data")
	}
	return value, nil
}

func keysAre(value map[string]json.RawMessage, keys ...string) bool {
	if len(value) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := value[key]; !ok {
			return false
		}
	}
	return true
}
func integer(raw json.RawMessage, min, max int64) (int64, bool) {
	var number int64
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &number) != nil || number < min || number > max {
		return 0, false
	}
	return number, true
}
func text(raw json.RawMessage) (string, bool) {
	var value string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}
func printable(value string) bool {
	for _, r := range value {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func (api *queueAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if api.fixture != nil && strings.HasPrefix(r.URL.Path, "/__fixture/") {
		api.fixture.ServeHTTP(w, r)
		return
	}
	readOnly := r.Method == http.MethodGet || r.Method == http.MethodHead
	if r.Method == http.MethodOptions {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS, POST")
		w.WriteHeader(200)
		return
	}
	if readOnly {
		switch r.URL.Path {
		case "/api/csrf":
			jsonResponse(w, 200, map[string]any{"token": csrf.Token(r)})
			return
		case "/api/status":
			api.invoke(w, r, []string{"status", "--json"}, nil)
			return
		case "/api/profiles":
			api.profiles(w, r)
			return
		case "/":
			api.asset(w, r, "index.html")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/static/") {
			api.asset(w, r, strings.TrimPrefix(r.URL.Path, "/static/"))
			return
		}
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "api" && parts[1] == "profiles" {
		if len(parts) == 3 && readOnly && profileID.MatchString(parts[2]) && r.URL.RawQuery == "" {
			api.invoke(w, r, []string{"profiles", "show", parts[2], "--json"}, nil)
			return
		}
		if len(parts) == 4 && r.Method == http.MethodPost {
			api.profileAction(w, r, parts[2], parts[3])
			return
		}
		jsonError(w, 404, "Unknown workload profile")
		return
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "jobs" {
		id, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || id < 0 || strings.HasPrefix(parts[2], "+") {
			jsonError(w, 404, "Unknown queue action")
			return
		}
		if readOnly && parts[3] == "log" {
			api.log(w, r, id)
			return
		}
		if r.Method == http.MethodPost && (parts[3] == "next" || parts[3] == "last" || parts[3] == "start" || parts[3] == "cancel") {
			value, ok := readObject(w, r, true)
			if !ok {
				return
			}
			if len(value) != 0 {
				jsonError(w, 400, "This action does not accept parameters")
				return
			}
			api.mutate(w, r, []string{parts[3], strconv.FormatInt(id, 10)})
			return
		}
		jsonError(w, 404, "Unknown queue action")
		return
	}
	if r.Method == http.MethodPost && (r.URL.Path == "/api/limit" || r.URL.Path == "/api/scheduling") {
		value, ok := readObject(w, r, false)
		if !ok {
			return
		}
		if r.URL.Path == "/api/limit" {
			limit, valid := integer(value["limit"], 1, 8)
			if !valid || !keysAre(value, "limit") {
				jsonError(w, 400, "Limit must be an integer from 1 to 8")
				return
			}
			api.mutate(w, r, []string{"limit", strconv.FormatInt(limit, 10)})
		} else {
			mode, valid := text(value["mode"])
			if !valid || !keysAre(value, "mode") || mode != "fixed" && mode != "shadow" && mode != "adaptive" {
				jsonError(w, 400, "Scheduling mode must be fixed, shadow, or adaptive")
				return
			}
			api.mutate(w, r, []string{"scheduling", mode})
		}
		return
	}
	jsonError(w, 404, "Not Found")
}

func (api *queueAPI) profiles(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		jsonError(w, 400, "Invalid profile pagination or search")
		return
	}
	for key, values := range query {
		if key != "offset" && key != "limit" && key != "query" || len(values) != 1 {
			jsonError(w, 400, "Unknown profile query parameter")
			return
		}
	}
	offset, limit, search := "0", "100", query.Get("query")
	if query.Has("offset") {
		offset = query.Get("offset")
	}
	if query.Has("limit") {
		limit = query.Get("limit")
	}
	n, _ := strconv.Atoi(limit)
	if !pageOffset.MatchString(offset) || !pageLimit.MatchString(limit) || n < 1 || n > 100 || utf8.RuneCountInString(search) > 120 || strings.ContainsFunc(search, func(r rune) bool { return r < 32 }) {
		jsonError(w, 400, "Invalid profile pagination or search")
		return
	}
	api.invoke(w, r, []string{"profiles", "list", "--offset", offset, "--limit", limit, "--query", search, "--json"}, nil)
}

func (api *queueAPI) profileAction(w http.ResponseWriter, r *http.Request, id, action string) {
	fields := map[string][]string{"edit": {"revision", "label", "memoryFloorBytes", "cpuFloorCores"}, "merge": {"revision", "targetId"}, "split": {"revision", "family"}, "reset": {"revision", "confirm"}}
	keys, known := fields[action]
	if !known || !profileID.MatchString(id) {
		jsonError(w, 404, "Unknown workload profile action")
		return
	}
	value, ok := readObject(w, r, false)
	if !ok {
		return
	}
	if _, valid := integer(value["revision"], 0, 1<<53-1); !valid || !keysAre(value, keys...) {
		jsonError(w, 400, "A current profile revision and valid action fields are required")
		return
	}
	switch action {
	case "edit":
		label, valid := text(value["label"])
		valid = valid && utf8.RuneCountInString(label) >= 1 && utf8.RuneCountInString(label) <= 120 && label == strings.TrimSpace(label) && printable(label)
		if !bytes.Equal(value["memoryFloorBytes"], []byte("null")) {
			_, memoryOK := integer(value["memoryFloorBytes"], 1, 1<<50)
			valid = valid && memoryOK
		}
		if !bytes.Equal(value["cpuFloorCores"], []byte("null")) {
			var cpu float64
			err := json.Unmarshal(value["cpuFloorCores"], &cpu)
			valid = valid && err == nil && cpu > 0 && cpu <= 1024 && !math.IsInf(cpu, 0) && !math.IsNaN(cpu)
		}
		if !valid {
			jsonError(w, 400, "Use a valid name and positive memory or CPU floors, or null for automatic")
			return
		}
	case "merge", "split":
		key := "targetId"
		if action == "split" {
			key = "family"
		}
		identifier, valid := text(value[key])
		if !valid || !profileID.MatchString(identifier) {
			jsonError(w, 400, "A valid workload identity is required")
			return
		}
	case "reset":
		if !bytes.Equal(value["confirm"], []byte("true")) {
			jsonError(w, 400, "Reset requires explicit confirmation")
			return
		}
	}
	input, err := json.Marshal(value)
	if err != nil {
		jsonError(w, 400, "Bad Request")
		return
	}
	api.invoke(w, r, []string{"profiles", action, id, "--json"}, input)
}

func (api *queueAPI) log(w http.ResponseWriter, r *http.Request, id int64) {
	status, err := api.run(r.Context(), []string{"status", "--json"}, nil)
	if err != nil {
		jsonError(w, 502, err.Error())
		return
	}
	// Normalize injected and decoded controller results through JSON numbers.
	data, err := json.Marshal(status["jobs"])
	var jobs []struct {
		ID      int64  `json:"id"`
		LogPath string `json:"logPath"`
	}
	if err != nil || json.Unmarshal(data, &jobs) != nil {
		jsonError(w, 502, "Queue controller returned invalid status")
		return
	}
	logPath := ""
	for _, job := range jobs {
		if job.ID == id {
			logPath = job.LogPath
			break
		}
	}
	if logPath == "" {
		jsonError(w, 404, "Job log is unavailable")
		return
	}
	if !api.testing {
		home, err := os.UserHomeDir()
		if err != nil {
			jsonError(w, 502, "Controller supplied an invalid log path")
			return
		}
		if !validQueueLogPath(home, logPath, id) {
			jsonError(w, 502, "Controller supplied an invalid log path")
			return
		}
	}
	fd, err := syscall.Open(logPath, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errors.Is(err, os.ErrNotExist) {
		jsonResponse(w, 200, map[string]any{"output": "", "truncated": false})
		return
	}
	if err != nil {
		jsonError(w, 502, "Job log ownership could not be verified")
		return
	}
	f := os.NewFile(uintptr(fd), logPath)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !ownedByCurrentUser(info) || info.Mode().Perm()&0o077 != 0 {
		jsonError(w, 502, "Job log ownership could not be verified")
		return
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
		jsonError(w, 502, "Job log ownership could not be verified")
		return
	}
	offset := max(int64(0), info.Size()-maxLog)
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		jsonError(w, 502, "Job log is unavailable")
		return
	}
	output, err := io.ReadAll(io.LimitReader(f, maxLog))
	if err != nil {
		jsonError(w, 502, "Job log is unavailable")
		return
	}
	jsonResponse(w, 200, map[string]any{"output": strings.ToValidUTF8(string(output), "\ufffd"), "truncated": offset > 0})
}

func validQueueLogPath(home, logPath string, id int64) bool {
	if filepath.Base(logPath) != strconv.FormatInt(id, 10)+".log" {
		return false
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(logPath))
	if err != nil {
		return false
	}
	// Imported completed jobs retain their original private log files.
	for _, relative := range []string{"logs", "pueue/task_logs"} {
		expected, err := filepath.EvalSymlinks(filepath.Join(home, ".local/state/agent-heavy", relative))
		if err == nil && parent == expected {
			return true
		}
	}
	return false
}

func (api *queueAPI) asset(w http.ResponseWriter, r *http.Request, name string) {
	if !fs.ValidPath(name) || name != path.Clean(name) || name != "index.html" && name != "app.css" && name != "app.js" && name != "profiles.js" && name != "schema.mjs" && name != "vendor/zod.mjs" && name != "vendor/ZOD-LICENSE" {
		jsonError(w, 404, "Not Found")
		return
	}
	data, err := fs.ReadFile(api.static, name)
	if err != nil {
		jsonError(w, 404, "Not Found")
		return
	}
	contentType := mime.TypeByExtension(path.Ext(name))
	if strings.HasSuffix(name, ".mjs") {
		contentType = "text/javascript; charset=utf-8"
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(200)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func coreExecutable() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	sibling := filepath.Join(filepath.Dir(self), "agent-heavy")
	if info, err := os.Stat(sibling); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
		return sibling, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	installed := filepath.Join(home, ".local/bin/agent-heavy")
	if info, err := os.Stat(installed); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
		return installed, nil
	}
	return "", errors.New("Queue controller is unavailable")
}

func coreRunner(ctx context.Context, arguments []string, input []byte) (map[string]any, error) {
	executable, err := coreExecutable()
	if err != nil {
		return nil, err
	}
	return executableRunner(executable)(ctx, arguments, input)
}

func executableRunner(executable string) Runner {
	return func(ctx context.Context, arguments []string, input []byte) (map[string]any, error) {
		ctx, cancel := context.WithTimeout(ctx, 23*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, arguments...)
		cmd.WaitDelay = time.Second
		if input != nil {
			cmd.Stdin = bytes.NewReader(input)
		}
		// Controller status can contain long queues; bounded output still prevents
		// an accidentally unbounded subprocess from exhausting the API process.
		output := &boundedBuffer{limit: 16 << 20}
		cmd.Stdout = output
		cmd.Stderr = io.Discard
		err := cmd.Run()
		if err != nil {
			var failure struct {
				Error string `json:"error"`
			}
			if len(arguments) > 0 && arguments[0] == "profiles" {
				fields, decodeErr := decodeObject(output.Bytes())
				if decodeErr == nil && keysAre(fields, "error") && json.Unmarshal(output.Bytes(), &failure) == nil && len(failure.Error) > 0 && len(failure.Error) <= 4096 {
					return nil, errors.New(failure.Error)
				}
			}
			if ctx.Err() != nil {
				return nil, errors.New("Queue controller is unavailable")
			}
			return nil, errors.New("Queue action failed; inspect the local Agent Queue")
		}
		if len(arguments) > 0 && (arguments[0] == "profiles" || len(arguments) == 2 && arguments[0] == "status" && arguments[1] == "--json") {
			var value map[string]any
			decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
			decoder.UseNumber()
			_, objectErr := decodeObject(output.Bytes())
			if objectErr != nil || decoder.Decode(&value) != nil || value == nil {
				return nil, errors.New("Queue controller returned invalid status")
			}
			if arguments[0] == "status" {
				if _, ok := value["jobs"].([]any); !ok {
					return nil, errors.New("Queue controller returned invalid status")
				}
			}
			return value, nil
		}
		return map[string]any{"ok": true}, nil
	}
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if len(data) > b.limit-b.Len() {
		return 0, fmt.Errorf("Controller output exceeds limit")
	}
	return b.Buffer.Write(data)
}
