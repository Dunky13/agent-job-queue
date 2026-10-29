package backend

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const laneUsageText = `Usage: agent-heavy lane <command> [--json]
  list
  acquire LANE --owner-pid PID [--ttl SECONDS] [--label TEXT] [--meta KEY=VALUE]... [--no-wait] [--timeout SECONDS]
  wait LEASE_ID [--timeout SECONDS]
  status LEASE_ID
  renew LEASE_ID
  release LEASE_ID [--owner-pid PID] [--force]
Exit status: 0 success, 1 lease gone or refused, 2 usage error, 75 timed out while still queued.
`
const pluginsUsageText = `Usage: agent-heavy plugins <command> [--json]
  list
  validate DIRECTORY
  reload
`

// Lane exit statuses follow the contract rather than the CLI's 125 default.
const (
	laneExitRefused  = 1
	laneExitUsage    = 2
	laneExitTimedOut = 75
)

var errLanesUnsupported = errors.New("This queue does not support lanes; update Agent Queue")

type laneOptions struct {
	json, noWait, force bool
	ownerPID, ttl       *int
	timeout             *time.Duration
	label               *string
	meta                Object
	positional          []string
}

func parseLaneOptions(args []string, allowed ...string) (laneOptions, error) {
	options := laneOptions{meta: Object{}}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			options.positional = append(options.positional, arg)
			continue
		}
		name, value, inline := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if !containsString(allowed, name) {
			return options, fmt.Errorf("Unknown option --%s", name)
		}
		switch name {
		case "json", "no-wait", "force":
			if inline {
				return options, fmt.Errorf("--%s takes no value", name)
			}
			options.json = options.json || name == "json"
			options.noWait = options.noWait || name == "no-wait"
			options.force = options.force || name == "force"
			continue
		}
		if !inline {
			if i+1 >= len(args) {
				return options, fmt.Errorf("--%s requires a value", name)
			}
			i++
			value = args[i]
		}
		switch name {
		case "owner-pid", "ttl", "timeout":
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return options, fmt.Errorf("--%s requires a nonnegative integer", name)
			}
			switch name {
			case "owner-pid":
				options.ownerPID = &n
			case "ttl":
				if err = validateLeaseTTL(n, false); err != nil {
					return options, err
				}
				options.ttl = &n
			case "timeout":
				if n > 7*24*3600 {
					return options, errors.New("--timeout is at most one week")
				}
				timeout := time.Duration(n) * time.Second
				options.timeout = &timeout
			}
		case "label":
			if err := validLeaseLabel(value); err != nil {
				return options, err
			}
			options.label = &value
		case "meta":
			key, text, ok := strings.Cut(value, "=")
			if !ok {
				return options, errors.New("--meta requires KEY=VALUE")
			}
			if _, duplicate := options.meta[key]; duplicate {
				return options, fmt.Errorf("--meta %s is given twice", key)
			}
			options.meta[key] = text
			if _, err := validLeaseMeta(options.meta); err != nil {
				return options, err
			}
		}
	}
	return options, nil
}

type laneOutput struct {
	stdout, stderr io.Writer
	json           bool
}

func (o laneOutput) result(code int, value Object, human string) int {
	if o.json {
		value["schemaVersion"] = 1
		_ = json.NewEncoder(o.stdout).Encode(value)
	} else if human != "" {
		fmt.Fprintln(o.stdout, human)
	}
	return code
}

func (o laneOutput) failure(code int, err error, value Object) int {
	if value == nil {
		value = Object{}
	}
	value["error"] = err.Error()
	if o.json {
		value["schemaVersion"] = 1
		_ = json.NewEncoder(o.stdout).Encode(value)
	} else {
		fmt.Fprintln(o.stderr, "agent-heavy:", err)
	}
	return code
}

// laneSnapshot proves the daemon publishes lanes before any lane operation;
// an older daemon would otherwise reject the request with an unrelated error.
func laneSnapshot(g *Gate) (Object, error) {
	if err := g.Prepare(); err != nil {
		return nil, err
	}
	snapshot, err := g.Snapshot()
	if err != nil {
		return nil, err
	}
	if obj(snapshot["lanes"]) == nil || snapshot["plugins"] == nil {
		return nil, errLanesUnsupported
	}
	return snapshot, nil
}

func laneNames(snapshot Object) []string {
	names := []string{}
	for _, lane := range sortedLanes(obj(snapshot["lanes"])) {
		if lane["removed"] != true {
			names = append(names, text(lane["name"]))
		}
	}
	return names
}

func unknownLaneError(snapshot Object, name string) error {
	return fmt.Errorf("%w %q; available lanes: %s", errQueueUnknownLane, name, strings.Join(laneNames(snapshot), ", "))
}

func laneExit(err error) int {
	if errors.Is(err, errQueueUnknownLane) {
		return laneExitUsage
	}
	return laneExitRefused
}

func describeLease(lease Object) string {
	parts := []string{fmt.Sprintf("lease %v %s in lane %s", lease["id"], text(lease["state"]), text(lease["lane"]))}
	if lease["position"] != nil {
		parts = append(parts, fmt.Sprintf("position %v", lease["position"]))
	}
	if label := text(lease["label"]); label != "" {
		parts = append(parts, label)
	}
	if lease["reason"] != nil {
		parts = append(parts, "reason "+text(lease["reason"]))
	}
	return strings.Join(parts, "; ")
}

func describeLeaseStatus(lease Object) string {
	lines := []string{}
	for _, key := range []string{"id", "lane", "state", "position", "label", "ownerPid", "ownerAlive", "queuedAt", "grantedAt", "renewedAt", "expiresAt", "reason"} {
		if value := lease[key]; value != nil && value != "" {
			lines = append(lines, fmt.Sprintf("%s: %v", key, value))
		}
	}
	meta := obj(lease["meta"])
	keys := make([]string, 0, len(meta))
	for key := range meta {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		lines = append(lines, fmt.Sprintf("meta.%s: %s", key, text(meta[key])))
	}
	return strings.Join(lines, "\n")
}

func leaseID(options laneOptions) (int, error) {
	if len(options.positional) != 1 {
		return 0, errors.New("Expected one lease ID")
	}
	id, err := strconv.Atoi(options.positional[0])
	if err != nil || id < 0 {
		return 0, errors.New("Lease ID must be a nonnegative integer")
	}
	return id, nil
}

// LaneMain implements the lane commands; it returns the contract's statuses.
func LaneMain(g *Gate, args []string, stdout, stderr io.Writer) int {
	out := laneOutput{stdout: stdout, stderr: stderr}
	if len(args) == 0 {
		fmt.Fprint(stderr, laneUsageText)
		return laneExitUsage
	}
	command := args[0]
	allowed := map[string][]string{
		"list":    {"json"},
		"acquire": {"json", "owner-pid", "ttl", "label", "meta", "no-wait", "timeout"},
		"wait":    {"json", "timeout"},
		"status":  {"json"},
		"renew":   {"json"},
		"release": {"json", "owner-pid", "force"},
	}[command]
	if allowed == nil {
		fmt.Fprint(stderr, laneUsageText)
		return laneExitUsage
	}
	options, err := parseLaneOptions(args[1:], allowed...)
	out.json = options.json || containsString(args[1:], "--json")
	if err != nil {
		return out.failure(laneExitUsage, err, nil)
	}
	snapshot, err := laneSnapshot(g)
	if err != nil {
		return out.failure(laneExitRefused, err, nil)
	}
	switch command {
	case "list":
		if len(options.positional) != 0 {
			return out.failure(laneExitUsage, errors.New("lane list takes no arguments"), nil)
		}
		lanes := sortedLanes(obj(snapshot["lanes"]))
		human := []string{}
		for _, lane := range lanes {
			line := fmt.Sprintf("%s (%s): priority %v, concurrency %v, %v running, %v held, %v waiting", text(lane["name"]), text(lane["label"]), lane["priority"], lane["concurrency"], lane["running"], lane["held"], lane["waiting"])
			if lane["sharesLimit"] == false {
				line += ", outside the parallel limit"
			}
			if lane["removed"] == true {
				line += ", removed"
			}
			human = append(human, line)
		}
		return out.result(0, Object{"lanes": lanes}, strings.Join(human, "\n"))
	case "acquire":
		return laneAcquire(g, snapshot, options, out)
	}
	id, err := leaseID(options)
	if err != nil {
		return out.failure(laneExitUsage, err, nil)
	}
	switch command {
	case "wait":
		return laneWait(g, id, options.timeout, out, nil)
	case "status":
		lease, err := g.QueueControl("lease-status", Object{"id": id})
		if err != nil {
			return out.failure(laneExit(err), err, Object{"id": id})
		}
		return out.result(0, lease, describeLeaseStatus(lease))
	case "renew":
		lease, err := g.QueueControl("lease-renew", Object{"id": id})
		if err != nil {
			return out.failure(laneExit(err), err, Object{"id": id})
		}
		return out.result(0, lease, describeLease(lease))
	default:
		payload := Object{"id": id, "force": options.force}
		if pid := leaseOwnerOption(options); pid != nil {
			payload["ownerPid"] = *pid
		}
		lease, err := g.QueueControl("lease-release", payload)
		if err != nil {
			return out.failure(laneExit(err), err, Object{"id": id})
		}
		return out.result(0, lease, describeLease(lease))
	}
}

// leaseOwnerOption lets an agent export its owner once instead of repeating
// --owner-pid on every acquire and release.
func leaseOwnerOption(options laneOptions) *int {
	if options.ownerPID != nil {
		return options.ownerPID
	}
	if value, ok := os.LookupEnv("AGENT_QUEUE_OWNER_PID"); ok {
		if pid, err := strconv.Atoi(value); err == nil && pid > 1 {
			return &pid
		}
	}
	return nil
}

func laneAcquire(g *Gate, snapshot Object, options laneOptions, out laneOutput) int {
	if len(options.positional) != 1 {
		return out.failure(laneExitUsage, errors.New("lane acquire requires one lane"), nil)
	}
	name := options.positional[0]
	owner := leaseOwnerOption(options)
	if owner == nil && options.ttl == nil {
		return out.failure(laneExitUsage, errors.New("lane acquire requires --owner-pid unless --ttl is given"), nil)
	}
	if lane := snapshotLane(snapshot, name); name == defaultLaneName || lane == nil || lane["removed"] == true {
		return out.failure(laneExitUsage, unknownLaneError(snapshot, name), nil)
	}
	payload := Object{"token": token(), "lane": name, "meta": options.meta}
	if owner != nil {
		payload["ownerPid"] = *owner
	}
	if options.ttl != nil {
		payload["ttlSeconds"] = *options.ttl
	}
	if options.label != nil {
		payload["label"] = *options.label
	}
	lease, err := g.QueueControl("lease-acquire", payload)
	// A lost response does not establish whether the lease was created; the
	// same token returns the original lease.
	for retry := 0; err != nil && errors.Is(err, errQueueTransportUnavailable) && retry < 2; retry++ {
		lease, err = g.QueueControl("lease-acquire", payload)
	}
	if err != nil {
		return out.failure(laneExit(err), err, nil)
	}
	if options.noWait || lease["state"] == "held" {
		return out.result(0, lease, describeLease(lease))
	}
	id, _ := integer(lease["id"])
	// Interrupting a blocking acquire abandons the request it created; a
	// bounded timeout keeps it queued instead.
	release := func() {
		release := Object{"id": id, "force": true}
		_, _ = g.QueueControl("lease-release", release)
	}
	return laneWait(g, id, options.timeout, out, release)
}

// laneWait polls the daemon in bounded steps; it never changes the lease, so a
// caller can wait repeatedly without losing its queue position.
func laneWait(g *Gate, id int, timeout *time.Duration, out laneOutput, interrupted func()) int {
	signals := make(chan os.Signal, 2)
	if interrupted != nil {
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		defer signal.Stop(signals)
	}
	var deadline <-chan time.Time
	if timeout != nil {
		timer := time.NewTimer(*timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	var last Object
	unavailableSince := time.Time{}
	for {
		lease, err := g.QueueControl("lease-status", Object{"id": id})
		switch {
		case err == nil:
			last = lease
			unavailableSince = time.Time{}
			switch text(lease["state"]) {
			case "held":
				return out.result(0, lease, describeLease(lease))
			case "queued":
			default:
				return out.failure(laneExitRefused, fmt.Errorf("Lease %d is %s (%s)", id, text(lease["state"]), text(lease["reason"])), lease)
			}
		case errors.Is(err, errQueueTransportUnavailable) || errors.Is(err, errQueueHandoverBusy):
			if unavailableSince.IsZero() {
				unavailableSince = time.Now()
			} else if time.Since(unavailableSince) > 30*time.Second {
				return out.failure(laneExitRefused, err, Object{"id": id})
			}
		default:
			return out.failure(laneExit(err), err, Object{"id": id})
		}
		select {
		case <-deadline:
			if last == nil {
				last = Object{"id": id, "state": "queued"}
			}
			return out.failure(laneExitTimedOut, fmt.Errorf("Lease %d is still queued; it keeps its position", id), last)
		case <-signals:
			interrupted()
			return out.failure(130, fmt.Errorf("Interrupted; lease %d was withdrawn", id), Object{"id": id})
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// PluginsMain implements the plugin commands with the lane exit statuses.
func PluginsMain(g *Gate, args []string, stdout, stderr io.Writer) int {
	out := laneOutput{stdout: stdout, stderr: stderr}
	if len(args) == 0 || !containsString([]string{"list", "validate", "reload"}, args[0]) {
		fmt.Fprint(stderr, pluginsUsageText)
		return laneExitUsage
	}
	options, err := parseLaneOptions(args[1:], "json")
	out.json = options.json || containsString(args[1:], "--json")
	if err != nil {
		return out.failure(laneExitUsage, err, nil)
	}
	if args[0] == "validate" {
		if len(options.positional) != 1 {
			return out.failure(laneExitUsage, errors.New("plugins validate requires one directory"), nil)
		}
		result, err := ValidatePluginDirectory(options.positional[0])
		if err != nil {
			return out.failure(laneExitRefused, err, nil)
		}
		human := fmt.Sprintf("Plugin %s is valid", text(result["name"]))
		code := 0
		if result["valid"] != true {
			code = laneExitRefused
			human = fmt.Sprintf("Plugin %s is invalid: %s", text(result["name"]), strings.Trim(text(result["error"])+"; "+text(result["warning"]), "; "))
		}
		return out.result(code, result, human)
	}
	if len(options.positional) != 0 {
		return out.failure(laneExitUsage, fmt.Errorf("plugins %s takes no arguments", args[0]), nil)
	}
	snapshot, err := laneSnapshot(g)
	if err != nil {
		return out.failure(laneExitRefused, err, nil)
	}
	if args[0] == "reload" {
		if snapshot, err = g.QueueControl("plugins-reload", Object{}); err != nil {
			return out.failure(laneExitRefused, err, nil)
		}
	}
	plugins := list(snapshot["plugins"])
	human := []string{}
	for _, raw := range plugins {
		plugin := obj(raw)
		line := fmt.Sprintf("%s: ", text(plugin["name"]))
		if plugin["loaded"] == true {
			line += "loaded"
			if lanes := list(plugin["lanes"]); len(lanes) > 0 {
				names := []string{}
				for _, lane := range lanes {
					names = append(names, text(lane))
				}
				line += " (lanes " + strings.Join(names, ", ") + ")"
			}
		} else {
			line += "not loaded"
		}
		for _, key := range []string{"error", "warning"} {
			if value := text(plugin[key]); value != "" {
				line += "; " + value
			}
		}
		human = append(human, line)
	}
	if len(human) == 0 {
		human = append(human, "No plugins installed in "+pluginRoot(g))
	}
	return out.result(0, Object{"plugins": plugins, "lanes": sortedLanes(obj(snapshot["lanes"]))}, strings.Join(human, "\n"))
}
