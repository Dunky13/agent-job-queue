package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"unicode"
)

const pluginManifestLimit = 64 * 1024
const pluginLimit = 32
const pluginLaneLimit = 8
const pluginHooksPerEvent = 4
const defaultLaneName = "default"

var pluginNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
var leaseMetaKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

var pluginEvents = []string{"job.queued", "job.started", "job.finished", "lease.queued", "lease.granted", "lease.released", "lease.expired", "lease.dropped"}

// queueLane is the daemon's scheduling view of one lane. The default lane is
// synthesized from the global limit and never comes from a manifest.
type queueLane struct {
	Name, Label, Plugin string
	Concurrency         int
	Priority            int
	SharesLimit         bool
	MemoryMiB           int
	CPUPercent          int
	TTLSeconds          int
	Removed             bool
}

type queuePlugin struct {
	Name    string
	Dir     string
	Loaded  bool
	Error   string
	Warning string
	Lanes   []string
	Hooks   map[string][]string
}

type pluginRegistry struct {
	lanes   map[string]queueLane
	plugins []queuePlugin
}

func pluginRoot(g *Gate) string { return filepath.Join(filepath.Dir(g.Config), "plugins") }

func pluginText(value string, limit int) error {
	if len([]rune(value)) > limit {
		return fmt.Errorf("must be at most %d characters", limit)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return errors.New("must not contain control characters")
	}
	return nil
}

// pluginDirectoryInfo applies the repository's configuration-directory posture:
// a real directory owned by this user that no other user can modify.
func pluginDirectoryInfo(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || int(stat.Uid) != os.Getuid() || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("Unsafe plugin directory %s: it must be a real directory owned by you and not group or world writable", path)
	}
	return nil
}

func pluginRegularInfo(path string, executable bool) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !fileOwned(info, false) || stat.Nlink != 1 || executable && info.Mode().Perm()&0o100 == 0 {
		kind := "regular file"
		if executable {
			kind = "regular executable file"
		}
		return nil, fmt.Errorf("Unsafe plugin file %s: it must be a single-link %s owned by you and not group or world writable", path, kind)
	}
	return info, nil
}

// readPluginManifest opens without following links and proves the opened file
// is the inode that passed the path checks.
func readPluginManifest(path string) ([]byte, error) {
	info, err := pluginRegularInfo(path, false)
	if err != nil {
		return nil, err
	}
	if info.Size() > pluginManifestLimit {
		return nil, errors.New("plugin.json exceeds 64 KiB")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, errors.New("plugin.json changed while it was opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, pluginManifestLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > pluginManifestLimit {
		return nil, errors.New("plugin.json exceeds 64 KiB")
	}
	return data, nil
}

// rejectDuplicateKeys walks the token stream because encoding/json keeps the
// last duplicate silently, which would let a typo override a reviewed value.
func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var walk func(depth int) error
	walk = func(depth int) error {
		if depth > 16 {
			return errors.New("JSON nesting is too deep")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, _ := key.(string)
				if seen[name] {
					return fmt.Errorf("duplicate key %q", name)
				}
				seen[name] = true
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		}
		_, err = decoder.Token()
		return err
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing content after the manifest")
	}
	return nil
}

// strictFields decodes an object whose keys must match exactly; encoding/json
// alone matches field names case-insensitively.
func strictFields(raw json.RawMessage, allowed []string, required ...string) (map[string]json.RawMessage, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, errors.New("expected an object, not null")
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, errors.New("expected an object")
	}
	for key, value := range fields {
		if !containsString(allowed, key) {
			return nil, fmt.Errorf("unknown field %q", key)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("field %q must not be null", key)
		}
	}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			return nil, fmt.Errorf("missing field %q", key)
		}
	}
	return fields, nil
}

func strictValue(fields map[string]json.RawMessage, key string, target any) error {
	raw, ok := fields[key]
	if !ok {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("field %q has the wrong type", key)
	}
	return nil
}

func strictRange(fields map[string]json.RawMessage, key string, target *int, low, high int) error {
	if err := strictValue(fields, key, target); err != nil {
		return err
	}
	if _, ok := fields[key]; ok && (*target < low || *target > high) {
		return fmt.Errorf("field %q must be from %d to %d", key, low, high)
	}
	return nil
}

// validatePluginHookPath accepts only relative paths that resolve inside the
// plugin directory through real, owned, non-writable directories.
func validatePluginHookPath(directory, relative string) (string, error) {
	if relative == "" || len(relative) > 256 || strings.ContainsRune(relative, 0) || filepath.IsAbs(relative) || filepath.Clean(relative) != relative {
		return "", fmt.Errorf("Hook path %q must be a clean relative path inside the plugin directory", relative)
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) > 8 {
		return "", fmt.Errorf("Hook path %q is nested too deeply", relative)
	}
	current := directory
	for index, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("Hook path %q must stay inside the plugin directory", relative)
		}
		current = filepath.Join(current, part)
		if index < len(parts)-1 {
			if err := pluginDirectoryInfo(current); err != nil {
				return "", err
			}
		}
	}
	if _, err := pluginRegularInfo(current, true); err != nil {
		return "", err
	}
	return current, nil
}

// parsePluginManifest validates one manifest; it never consults other plugins.
func parsePluginManifest(directory string, data []byte) (queuePlugin, []queueLane, error) {
	name := filepath.Base(directory)
	plugin := queuePlugin{Name: name, Dir: directory, Hooks: map[string][]string{}, Lanes: []string{}}
	if err := rejectDuplicateKeys(data); err != nil {
		return plugin, nil, fmt.Errorf("Invalid plugin.json: %w", err)
	}
	top, err := strictFields(data, []string{"schemaVersion", "name", "description", "lanes", "hooks"}, "schemaVersion", "name", "lanes")
	if err != nil {
		return plugin, nil, fmt.Errorf("Invalid plugin.json: %w", err)
	}
	var version int
	if err = strictValue(top, "schemaVersion", &version); err != nil || version != 1 {
		return plugin, nil, errors.New("Invalid plugin.json: schemaVersion must be 1")
	}
	var declared, description string
	if err = strictValue(top, "name", &declared); err != nil {
		return plugin, nil, fmt.Errorf("Invalid plugin.json: %w", err)
	}
	if !pluginNamePattern.MatchString(declared) || declared != name {
		return plugin, nil, fmt.Errorf("Invalid plugin.json: name must match ^[a-z][a-z0-9-]{0,31}$ and equal the directory name %q", name)
	}
	if err = strictValue(top, "description", &description); err != nil {
		return plugin, nil, fmt.Errorf("Invalid plugin.json: %w", err)
	}
	if err = pluginText(description, 200); err != nil {
		return plugin, nil, fmt.Errorf("Invalid plugin.json: description %w", err)
	}
	var rawLanes []json.RawMessage
	if err = strictValue(top, "lanes", &rawLanes); err != nil {
		return plugin, nil, fmt.Errorf("Invalid plugin.json: %w", err)
	}
	if len(rawLanes) == 0 || len(rawLanes) > pluginLaneLimit {
		return plugin, nil, fmt.Errorf("Invalid plugin.json: lanes must list 1 to %d lanes", pluginLaneLimit)
	}
	lanes := []queueLane{}
	seen := map[string]bool{}
	for index, raw := range rawLanes {
		lane, err := parsePluginLane(raw)
		if err != nil {
			return plugin, nil, fmt.Errorf("Invalid plugin.json: lanes[%d]: %w", index, err)
		}
		if seen[lane.Name] {
			return plugin, nil, fmt.Errorf("Invalid plugin.json: lane %q is declared twice", lane.Name)
		}
		seen[lane.Name] = true
		lane.Plugin = name
		lanes = append(lanes, lane)
	}
	if raw, ok := top["hooks"]; ok {
		events, err := strictFields(raw, pluginEvents)
		if err != nil {
			return plugin, nil, fmt.Errorf("Invalid plugin.json: hooks: %w", err)
		}
		for event := range events {
			var paths []string
			if err = strictValue(events, event, &paths); err != nil {
				return plugin, nil, fmt.Errorf("Invalid plugin.json: hooks: %w", err)
			}
			if len(paths) > pluginHooksPerEvent {
				return plugin, nil, fmt.Errorf("Invalid plugin.json: hooks: at most %d hooks per event", pluginHooksPerEvent)
			}
			plugin.Hooks[event] = paths
		}
	}
	return plugin, lanes, nil
}

func parsePluginLane(raw json.RawMessage) (queueLane, error) {
	lane := queueLane{SharesLimit: true}
	fields, err := strictFields(raw, []string{"name", "label", "concurrency", "priority", "sharesLimit", "reservation", "lease"}, "name", "concurrency")
	if err != nil {
		return lane, err
	}
	if err = strictValue(fields, "name", &lane.Name); err != nil {
		return lane, err
	}
	if !pluginNamePattern.MatchString(lane.Name) {
		return lane, errors.New("name must match ^[a-z][a-z0-9-]{0,31}$")
	}
	if lane.Name == defaultLaneName {
		return lane, errors.New("lane name default is reserved")
	}
	lane.Label = lane.Name
	if _, ok := fields["label"]; ok {
		if err = strictValue(fields, "label", &lane.Label); err != nil {
			return lane, err
		}
		if lane.Label == "" {
			return lane, errors.New("label must not be empty")
		}
		if err = pluginText(lane.Label, 40); err != nil {
			return lane, fmt.Errorf("label %w", err)
		}
	}
	if err = strictRange(fields, "concurrency", &lane.Concurrency, 1, MaxParallel); err != nil {
		return lane, err
	}
	if err = strictRange(fields, "priority", &lane.Priority, -100, 100); err != nil {
		return lane, err
	}
	if err = strictValue(fields, "sharesLimit", &lane.SharesLimit); err != nil {
		return lane, err
	}
	if raw, ok := fields["reservation"]; ok {
		reservation, err := strictFields(raw, []string{"memoryMiB", "cpuPercent"})
		if err != nil {
			return lane, fmt.Errorf("reservation: %w", err)
		}
		if err = strictRange(reservation, "memoryMiB", &lane.MemoryMiB, 0, 65536); err != nil {
			return lane, fmt.Errorf("reservation: %w", err)
		}
		if err = strictRange(reservation, "cpuPercent", &lane.CPUPercent, 0, 800); err != nil {
			return lane, fmt.Errorf("reservation: %w", err)
		}
	}
	if raw, ok := fields["lease"]; ok {
		lease, err := strictFields(raw, []string{"ttlSeconds"})
		if err != nil {
			return lane, fmt.Errorf("lease: %w", err)
		}
		if err = strictValue(lease, "ttlSeconds", &lane.TTLSeconds); err != nil {
			return lane, fmt.Errorf("lease: %w", err)
		}
		if validateLeaseTTL(lane.TTLSeconds, true) != nil {
			return lane, errors.New("lease: ttlSeconds must be 0 or from 60 to 86400")
		}
	}
	return lane, nil
}

func validateLeaseTTL(value int, zero bool) error {
	if zero && value == 0 || value >= 60 && value <= 86400 {
		return nil
	}
	return errors.New("TTL must be from 60 to 86400 seconds")
}

// loadPluginDirectory checks one plugin end to end, including every hook. A
// failing hook is skipped and reported; it does not unload the plugin's lanes.
func loadPluginDirectory(directory string) (queuePlugin, []queueLane) {
	plugin := queuePlugin{Name: filepath.Base(directory), Dir: directory, Lanes: []string{}, Hooks: map[string][]string{}}
	if !pluginNamePattern.MatchString(plugin.Name) {
		plugin.Error = "Plugin directory name must match ^[a-z][a-z0-9-]{0,31}$"
		return plugin, nil
	}
	if err := pluginDirectoryInfo(directory); err != nil {
		plugin.Error = err.Error()
		return plugin, nil
	}
	data, err := readPluginManifest(filepath.Join(directory, "plugin.json"))
	if err != nil {
		plugin.Error = "Cannot load plugin.json: " + err.Error()
		return plugin, nil
	}
	parsed, lanes, err := parsePluginManifest(directory, data)
	if err != nil {
		plugin.Error = err.Error()
		return plugin, nil
	}
	plugin.Loaded = true
	warnings := []string{}
	events := make([]string, 0, len(parsed.Hooks))
	for event := range parsed.Hooks {
		events = append(events, event)
	}
	sort.Strings(events)
	for _, event := range events {
		for _, relative := range parsed.Hooks[event] {
			if _, err := validatePluginHookPath(directory, relative); err != nil {
				warnings = append(warnings, fmt.Sprintf("%s hook skipped: %v", event, err))
				continue
			}
			plugin.Hooks[event] = append(plugin.Hooks[event], relative)
		}
	}
	plugin.Warning = strings.Join(warnings, "; ")
	return plugin, lanes
}

// loadPluginRegistry never fails as a whole: problems are reported per plugin
// so the default lane and unrelated plugins keep working.
func loadPluginRegistry(g *Gate) pluginRegistry {
	registry := pluginRegistry{lanes: map[string]queueLane{}, plugins: []queuePlugin{}}
	root := pluginRoot(g)
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return registry
	}
	for _, directory := range []string{filepath.Dir(root), root} {
		if err := pluginDirectoryInfo(directory); err != nil {
			registry.plugins = append(registry.plugins, queuePlugin{Name: "plugins", Error: err.Error(), Lanes: []string{}})
			return registry
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		registry.plugins = append(registry.plugins, queuePlugin{Name: "plugins", Error: "Cannot read the plugins directory: " + err.Error(), Lanes: []string{}})
		return registry
	}
	owners := map[string][]int{}
	declared := [][]queueLane{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if len(registry.plugins) == pluginLimit {
			registry.plugins = append(registry.plugins, queuePlugin{Name: DisplayText(entry.Name(), 32), Error: fmt.Sprintf("At most %d plugins are loaded", pluginLimit), Lanes: []string{}})
			declared = append(declared, nil)
			continue
		}
		plugin, lanes := loadPluginDirectory(filepath.Join(root, entry.Name()))
		if !pluginNamePattern.MatchString(plugin.Name) {
			plugin.Name = DisplayText(plugin.Name, 32)
		}
		for _, lane := range lanes {
			owners[lane.Name] = append(owners[lane.Name], len(registry.plugins))
		}
		registry.plugins = append(registry.plugins, plugin)
		declared = append(declared, lanes)
	}
	for index, lanes := range declared {
		problems := []string{}
		for _, lane := range lanes {
			if len(owners[lane.Name]) > 1 {
				others := []string{}
				for _, owner := range owners[lane.Name] {
					if owner != index {
						others = append(others, registry.plugins[owner].Name)
					}
				}
				problems = append(problems, fmt.Sprintf("Lane %q collides with plugin %s; the lane is disabled", lane.Name, strings.Join(others, ", ")))
				continue
			}
			registry.lanes[lane.Name] = lane
			registry.plugins[index].Lanes = append(registry.plugins[index].Lanes, lane.Name)
		}
		if len(problems) > 0 {
			registry.plugins[index].Error = strings.Join(problems, "; ")
		}
	}
	return registry
}

// ValidatePluginDirectory runs the load-time checks on a directory anywhere on
// disk, so a plugin can be checked before it is installed.
func ValidatePluginDirectory(directory string) (Object, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	plugin, lanes := loadPluginDirectory(filepath.Clean(absolute))
	result := Object{"name": plugin.Name, "valid": plugin.Loaded, "error": nil, "warning": nil, "lanes": []Object{}}
	if plugin.Error != "" {
		result["error"] = plugin.Error
	}
	if plugin.Warning != "" {
		result["warning"] = plugin.Warning
		result["valid"] = false
	}
	described := []Object{}
	for _, lane := range lanes {
		described = append(described, laneDefinition(lane))
	}
	result["lanes"] = described
	return result, nil
}

func laneDefinition(lane queueLane) Object {
	var plugin any
	if lane.Plugin != "" {
		plugin = lane.Plugin
	}
	return Object{"name": lane.Name, "label": lane.Label, "plugin": plugin, "concurrency": lane.Concurrency, "priority": lane.Priority, "sharesLimit": lane.SharesLimit, "reservation": Object{"memoryMiB": lane.MemoryMiB, "cpuPercent": lane.CPUPercent}, "ttlSeconds": lane.TTLSeconds, "removed": lane.Removed}
}

func pluginStatus(plugin queuePlugin) Object {
	var errorText, warning any
	if plugin.Error != "" {
		errorText = plugin.Error
	}
	if plugin.Warning != "" {
		warning = plugin.Warning
	}
	lanes := append([]string{}, plugin.Lanes...)
	return Object{"name": plugin.Name, "loaded": plugin.Loaded, "error": errorText, "warning": warning, "lanes": lanes}
}
