package backend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const GiB = 1024 * 1024 * 1024
const profileDay = 86400

var profileHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
var profileDefinitions = []string{"package.json", "package-lock.json", "pnpm-lock.yaml", "pnpm-workspace.yaml", "yarn.lock", "bun.lock", "pyproject.toml", "uv.lock", "requirements.txt", "requirements.lock", "go.mod", "go.sum", "go.work", "Cargo.toml", "Cargo.lock", "Makefile", "justfile", "tox.ini", "pytest.ini", "setup.cfg", "setup.py", "tsconfig.json", "vite.config.ts", "vitest.config.ts", ".nvmrc", ".python-version", "rust-toolchain.toml"}

func profileDigest(value any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(profileByteSafeValue(value))
	raw := strings.TrimSuffix(buf.String(), "\n")
	var ascii strings.Builder
	for _, r := range raw {
		if r > 127 {
			if r <= 0xffff {
				fmt.Fprintf(&ascii, "\\u%04x", r)
			} else {
				a, b := utf16.EncodeRune(r)
				fmt.Fprintf(&ascii, "\\u%04x\\u%04x", a, b)
			}
		} else {
			ascii.WriteRune(r)
		}
	}
	sum := sha256.Sum256([]byte(ascii.String()))
	return hex.EncodeToString(sum[:])
}

// Keep every existing UTF-8 identity unchanged. Unix byte strings need an
// explicit representation before JSON encoding, otherwise different commands
// collapse to the same replacement character and share learned admission data.
func profileByteSafeValue(value any) any {
	switch v := value.(type) {
	case string:
		if !utf8.ValidString(v) {
			return Object{"unix_bytes": []byte(v)}
		}
	case []string:
		if v != nil {
			result := make([]any, len(v))
			for i, item := range v {
				result[i] = profileByteSafeValue(item)
			}
			return result
		}
	case []any:
		if v != nil {
			result := make([]any, len(v))
			for i, item := range v {
				result[i] = profileByteSafeValue(item)
			}
			return result
		}
	case map[string]string:
		if v != nil {
			result := Object{}
			for key, item := range v {
				result[key] = item
			}
			return profileByteSafeMap(result)
		}
	case Object:
		if v != nil {
			return profileByteSafeMap(v)
		}
	}
	return value
}

func profileByteSafeMap(value Object) any {
	keys := make([]string, 0, len(value))
	hasByteName := false
	result := Object{}
	for key, item := range value {
		keys = append(keys, key)
		hasByteName = hasByteName || !utf8.ValidString(key)
		result[key] = profileByteSafeValue(item)
	}
	if !hasByteName {
		return result
	}
	sort.Strings(keys)
	entries := make([]any, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, []any{[]byte(key), result[key]})
	}
	return Object{"unix_byte_entries": entries}
}
func profileGit(cwd string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0"}
	out, e := cmd.Output()
	if e != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
func profileRelative(value, root, alias string) string {
	prefixes := []string{root, alias}
	sort.SliceStable(prefixes, func(i, j int) bool { return len(prefixes[i]) > len(prefixes[j]) })
	for _, p := range prefixes {
		if value == p {
			return "<repo>"
		}
		value = strings.ReplaceAll(value, p+string(os.PathSeparator), "<repo>/")
		if strings.HasSuffix(value, "="+p) {
			value = strings.TrimSuffix(value, p) + "<repo>"
		}
	}
	return value
}
func profileFingerprint(path string) (any, error) {
	info, e := os.Lstat(path)
	if os.IsNotExist(e) {
		return []any{"missing"}, nil
	}
	if e != nil {
		return nil, fmt.Errorf("Cannot read workload provenance")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, e := os.Readlink(path)
		if e != nil {
			return nil, fmt.Errorf("Cannot read workload provenance")
		}
		return []any{"link", profileDigest(target)}, nil
	}
	if !info.Mode().IsRegular() {
		return []any{"other"}, nil
	}
	if info.Size() > 1024*1024 {
		return []any{"large", info.Size(), info.ModTime().UnixNano()}, nil
	}
	handle, e := os.Open(path)
	if e != nil {
		return nil, fmt.Errorf("Cannot read workload provenance")
	}
	defer handle.Close()
	data, e := io.ReadAll(io.LimitReader(handle, 1024*1024+1))
	if e != nil {
		return nil, fmt.Errorf("Cannot read workload provenance")
	}
	hash := sha256.Sum256(data)
	return []any{"file", hex.EncodeToString(hash[:])}, nil
}
func profileStatIdentity(info os.FileInfo) (uint64, uint64) {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return uint64(s.Dev), s.Ino
}
func profileToolchain(argv []string, cwd string, env map[string]string) ([]any, error) {
	selected := argv[0]
	executable := ""
	if strings.ContainsRune(selected, os.PathSeparator) {
		executable = selected
		if !filepath.IsAbs(executable) {
			executable = filepath.Join(cwd, executable)
		}
	} else {
		path, ok := env["PATH"]
		if !ok {
			path = os.Getenv("PATH")
		}
		for _, directory := range filepath.SplitList(path) {
			if directory == "" {
				directory = "."
			}
			candidate := filepath.Join(directory, selected)
			info, e := os.Stat(candidate)
			if e == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
				executable = candidate
				break
			}
		}
		if executable == "" {
			return []any{"unresolved", selected}, nil
		}
	}
	resolved, e := filepath.EvalSymlinks(executable)
	if os.IsNotExist(e) {
		return []any{"missing", selected}, nil
	}
	if e != nil {
		return nil, fmt.Errorf("Cannot inspect the workload toolchain")
	}
	resolved, e = filepath.Abs(resolved)
	if e != nil {
		return nil, e
	}
	info, e := os.Stat(resolved)
	if os.IsNotExist(e) {
		return []any{"missing", selected}, nil
	}
	if e != nil {
		return nil, fmt.Errorf("Cannot inspect the workload toolchain")
	}
	dev, ino := profileStatIdentity(info)
	return []any{resolved, dev, ino, info.Size(), info.ModTime().UnixNano()}, nil
}

var pythonInterpreter = regexp.MustCompile(`^(?:python|pypy)(?:\d+(?:\.\d+)*)?$`)
var pythonInline = regexp.MustCompile(`^-[BEIOPqsuUvWx]+c$`)
var shellInline = regexp.MustCompile(`^-[a-zA-Z]*c[a-zA-Z]*$`)

func profileProvenanceArguments(argv []string) []string {
	result := []string{}
	interpreter := ""
	for index, arg := range argv {
		name := filepath.Base(arg)
		switch {
		case pythonInterpreter.MatchString(name):
			interpreter = "python"
		case containsString([]string{"sh", "bash", "zsh", "dash", "ksh", "fish"}, name):
			interpreter = "shell"
		case containsString([]string{"node", "nodejs", "bun", "deno"}, name):
			interpreter = "javascript"
		case interpreter != "":
			inline := interpreter == "python" && (strings.HasPrefix(arg, "-c") || pythonInline.MatchString(arg))
			inline = inline || (interpreter == "shell" && (arg == "--command" || strings.HasPrefix(arg, "--command=") || shellInline.MatchString(arg)))
			inline = inline || (interpreter == "javascript" && (containsString([]string{"-e", "--eval", "-p", "--print", "eval"}, arg) || strings.HasPrefix(arg, "--eval=") || strings.HasPrefix(arg, "--print=") || strings.HasPrefix(arg, "-e")))
			if inline {
				return result
			}
		}
		if index > 0 && (!strings.HasPrefix(arg, "-") || strings.Contains(arg, "=")) {
			parts := strings.SplitN(arg, "=", 2)
			result = append(result, parts[len(parts)-1])
		}
	}
	return result
}
func containsString(values []string, needle string) bool {
	for _, v := range values {
		if v == needle {
			return true
		}
	}
	return false
}
func profileUnder(path, root string) (string, bool) {
	relative, e := filepath.Rel(root, path)
	return relative, e == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}
func IdentifyWorkload(argv []string, cwd string, env map[string]string) (Object, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("A workload needs a valid argument vector")
	}
	for _, arg := range argv {
		if strings.ContainsRune(arg, 0) {
			return nil, fmt.Errorf("A workload needs a valid argument vector")
		}
	}
	absolute, e := filepath.Abs(cwd)
	if e != nil {
		return nil, e
	}
	directory, e := filepath.EvalSymlinks(absolute)
	if e != nil {
		return nil, fmt.Errorf("Workload directory does not exist")
	}
	info, e := os.Stat(directory)
	if e != nil || !info.IsDir() {
		return nil, fmt.Errorf("Workload directory does not exist")
	}
	root := directory
	var revision any
	var repository any
	location := strings.Split(profileGit(directory, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir"), "\n")
	if len(location) == 2 {
		root, e = filepath.EvalSymlinks(location[0])
		if e != nil {
			return nil, fmt.Errorf("Cannot identify the Git repository")
		}
		common, e := filepath.EvalSymlinks(location[1])
		if e != nil {
			return nil, fmt.Errorf("Cannot identify the Git repository")
		}
		info, e := os.Stat(common)
		if e != nil {
			return nil, fmt.Errorf("Cannot identify the Git repository")
		}
		dev, ino := profileStatIdentity(info)
		repository = []any{"git", dev, ino}
		rev := profileGit(directory, "rev-parse", "HEAD")
		if rev != "" {
			revision = rev
		}
	}
	if repository == nil {
		repository = []any{"directory", directory}
	}
	relative, under := profileUnder(directory, root)
	if !under {
		return nil, fmt.Errorf("Workload is outside its Git checkout")
	}
	alias := absolute
	if relative != "." {
		for range strings.Split(relative, string(os.PathSeparator)) {
			alias = filepath.Dir(alias)
		}
	}
	command := make([]string, len(argv))
	for i, arg := range argv {
		command[i] = profileRelative(arg, root, alias)
	}
	executable := filepath.Base(argv[0])
	supported := []string{"go", "cargo", "make", "gmake", "npm", "pnpm", "yarn", "npx", "node", "python", "python3", "pytest", "uv"}
	if containsString(append(append([]string{}, supported...), "bash", "sh", "zsh"), executable) {
		command[0] = executable
	}
	workers := map[string]bool{}
	if containsString(supported, executable) {
		for _, flag := range []string{"--max-workers", "--maxWorkers", "--workers", "--jobs", "--threads", "--concurrency", "--test-concurrency"} {
			workers[flag] = true
		}
	}
	if executable == "go" {
		workers["-p"] = true
		workers["-parallel"] = true
	}
	if containsString([]string{"make", "gmake", "cargo"}, executable) {
		workers["-j"] = true
	}
	if executable == "pytest" {
		workers["-n"] = true
	}
	opaque := false
	if containsString([]string{"bash", "sh", "zsh"}, executable) {
		for _, arg := range argv[1:] {
			if strings.HasPrefix(arg, "-") && strings.Contains(arg[1:], "c") {
				opaque = true
			}
		}
	}
	familyCommand := []string{}
	skip := false
	for _, arg := range command {
		if skip {
			skip = false
			continue
		}
		flag := strings.SplitN(arg, "=", 2)[0]
		if len(workers) > 0 && containsString([]string{"--runInBand", "--run-in-band", "--no-file-parallelism"}, arg) {
			continue
		}
		attached := false
		for _, short := range []string{"-j", "-p", "-n"} {
			if workers[short] && strings.HasPrefix(arg, short) && len(arg) > len(short) {
				_, e := strconv.ParseUint(arg[len(short):], 10, 64)
				attached = attached || e == nil
			}
		}
		if !opaque && (workers[flag] || attached) {
			skip = !strings.Contains(arg, "=") && !attached
		} else {
			familyCommand = append(familyCommand, arg)
		}
	}
	explicit, hasExplicit := env["AGENT_HEAVY_PROFILE"]
	if hasExplicit && (strings.TrimSpace(explicit) == "" || len([]rune(explicit)) > 256) {
		return nil, fmt.Errorf("Explicit workload profile must contain 1 to 256 characters")
	}
	var familyArgs any = familyCommand
	if explicit != "" {
		familyArgs = []any{"explicit", explicit}
	}
	family := profileDigest([]any{"family-v1", repository, relative, familyArgs})
	provenance := []any{}
	if repo, ok := repository.([]any); ok && repo[0] == "git" {
		for parent := directory; ; parent = filepath.Dir(parent) {
			for _, name := range profileDefinitions {
				path := filepath.Join(parent, name)
				if _, e := os.Lstat(path); e == nil {
					finger, e := profileFingerprint(path)
					if e != nil {
						return nil, e
					}
					relative, _ := filepath.Rel(root, path)
					provenance = append(provenance, []any{relative, finger})
				}
			}
			if parent == root {
				break
			}
		}
	}
	for _, value := range profileProvenanceArguments(argv[:min(33, len(argv))]) {
		path := value
		if !filepath.IsAbs(path) {
			path = filepath.Join(directory, path)
		}
		resolved, e := profileResolveOptional(path)
		if e != nil {
			if os.IsNotExist(e) {
				continue
			}
			if errorsOptionalPath(e) {
				continue
			}
			return nil, fmt.Errorf("Cannot inspect workload argument provenance")
		}
		rel, inside := profileUnder(resolved, root)
		if !inside {
			continue
		}
		info, e := os.Lstat(path)
		if e != nil {
			if errorsOptionalPath(e) {
				continue
			}
			return nil, fmt.Errorf("Cannot inspect workload argument provenance")
		}
		if info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			finger, e := profileFingerprint(path)
			if e != nil {
				return nil, e
			}
			provenance = append(provenance, []any{rel, finger})
		}
	}
	environment := map[string]string{}
	for key, value := range env {
		if containsString([]string{"PWD", "OLDPWD", "SHLVL", "_", "TMPDIR", "TERM", "TERM_PROGRAM", "TERM_SESSION_ID"}, key) || strings.HasPrefix(key, "AGENT_HEAVY_") || strings.HasPrefix(key, "PUEUE_") {
			continue
		}
		environment[key] = profileRelative(value, root, alias)
	}
	toolchain, e := profileToolchain(argv, directory, env)
	if e != nil {
		return nil, e
	}
	if selected, ok := toolchain[0].(string); ok {
		toolchain[0] = profileRelative(selected, root, alias)
	}
	variant := profileDigest([]any{"variant-v1", family, command, revision, provenance, environment, toolchain})
	caches := []any{}
	for _, name := range []string{"node_modules", ".venv", "target", "build", ".pytest_cache", ".next", ".turbo"} {
		info, e := os.Stat(filepath.Join(directory, name))
		if os.IsNotExist(e) {
			caches = append(caches, []any{name, nil})
		} else if e != nil {
			return nil, fmt.Errorf("Cannot inspect workload cache context")
		} else {
			caches = append(caches, []any{name, info.ModTime().UnixNano(), info.Size()})
		}
	}
	return Object{"family": family, "variant": variant, "context": profileDigest([]any{"context-v1", caches})}, nil
}
func errorsOptionalPath(e error) bool {
	return os.IsNotExist(e) || strings.Contains(e.Error(), "not a directory") || strings.Contains(e.Error(), "file name too long")
}
func validProfileIdentity(identity Object) bool {
	for _, key := range []string{"family", "variant", "context"} {
		value, ok := identity[key].(string)
		if !ok || !profileHash.MatchString(value) {
			return false
		}
	}
	return true
}

// profileResolveOptional matches Path.resolve(strict=False) for broken links and missing leaves.
func profileResolveOptional(path string) (string, error) {
	path, e := filepath.Abs(path)
	if e != nil {
		return "", e
	}
	return profileResolveDepth(path, 0)
}
func profileResolveDepth(path string, depth int) (string, error) {
	if depth > 40 {
		return "", fmt.Errorf("Cannot inspect workload argument provenance")
	}
	resolved, e := filepath.EvalSymlinks(path)
	if e == nil {
		return resolved, nil
	}
	if !os.IsNotExist(e) {
		return "", e
	}
	info, statError := os.Lstat(path)
	if statError == nil && info.Mode()&os.ModeSymlink != 0 {
		target, e := os.Readlink(path)
		if e != nil {
			return "", e
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		return profileResolveDepth(target, depth+1)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path, nil
	}
	resolved, e = profileResolveDepth(parent, depth+1)
	if e != nil {
		return "", e
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}
