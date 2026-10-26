package backend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

type installRecoveryAction struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Destructive bool   `json:"destructive"`
}

type installRecoveryJob struct {
	ID     int    `json:"id"`
	Label  string `json:"label"`
	Status string `json:"status"`
}

type installPreflightIssue struct {
	ID      string                  `json:"id"`
	Title   string                  `json:"title"`
	Detail  string                  `json:"detail"`
	Path    string                  `json:"path,omitempty"`
	Actions []installRecoveryAction `json:"actions"`
	Jobs    []installRecoveryJob    `json:"jobs,omitempty"`
}

type installPreflightReport struct {
	SchemaVersion int                     `json:"schemaVersion"`
	Revision      string                  `json:"revision,omitempty"`
	Issues        []installPreflightIssue `json:"issues"`
}

type installOptions struct {
	target             []string
	preflight          bool
	disablePhoneAccess bool
	repairAndContinue  bool
	recovery           string
	jobID              *int
	revision           string
}

func installParseOptions(args []string) (installOptions, error) {
	options := installOptions{}
	usage := errors.New("Usage: agent-heavy install (--payload DIRECTORY | --app-bundle ABSOLUTE_APP_PATH) [--preflight --json | --recover ACTION [--job-id ID] [--expected-revision SHA] | --disable-phone-access | --repair-and-continue]")
	if len(args) < 2 || (args[0] != "--payload" && args[0] != "--app-bundle") || !filepath.IsAbs(args[1]) {
		return options, usage
	}
	options.target = append([]string{}, args[:2]...)
	seen := map[string]bool{}
	for i := 2; i < len(args); i++ {
		flag := args[i]
		if seen[flag] {
			return options, usage
		}
		seen[flag] = true
		switch flag {
		case "--preflight":
			options.preflight = true
		case "--json":
		case "--disable-phone-access":
			options.disablePhoneAccess = true
		case "--repair-and-continue":
			options.repairAndContinue = true
		case "--recover", "--job-id", "--expected-revision":
			if i+1 >= len(args) {
				return options, usage
			}
			i++
			value := args[i]
			if value == "" || strings.HasPrefix(value, "--") {
				return options, usage
			}
			switch flag {
			case "--recover":
				options.recovery = value
			case "--expected-revision":
				options.revision = value
			case "--job-id":
				id, err := strconv.Atoi(value)
				if err != nil || id < 0 {
					return options, usage
				}
				options.jobID = &id
			}
		default:
			return options, usage
		}
	}
	if options.preflight != seen["--json"] || options.preflight && (options.recovery != "" || options.disablePhoneAccess) || options.recovery != "" && options.disablePhoneAccess || options.recovery == "" && (options.jobID != nil || options.revision != "") {
		return options, usage
	}
	if options.repairAndContinue && (options.preflight || options.recovery != "" || options.disablePhoneAccess) {
		return options, usage
	}
	if options.recovery != "" && !slices.Contains([]string{"cancel-job", "cleanup-job", "repair-service", "disable-phone-access", "reset-callback", installRestoreServices}, options.recovery) {
		return options, usage
	}
	if options.recovery != "" && ((options.recovery == "cancel-job" || options.recovery == "cleanup-job") != (options.jobID != nil)) {
		return options, usage
	}
	if options.recovery == "reset-callback" {
		if len(options.revision) != 64 {
			return options, usage
		}
		if _, err := hex.DecodeString(options.revision); err != nil {
			return options, usage
		}
	} else if options.revision != "" {
		return options, usage
	}
	return options, nil
}

func installAction(id string) installRecoveryAction {
	values := map[string]installRecoveryAction{
		"wait":                 {ID: "wait", Title: "Wait and continue"},
		"review-jobs":          {ID: "review-jobs", Title: "Review jobs"},
		"cancel-job":           {ID: "cancel-job", Title: "Cancel selected job", Destructive: true},
		"cleanup-job":          {ID: "cleanup-job", Title: "Run verified cleanup"},
		"repair-service":       {ID: "repair-service", Title: "Start verified service"},
		installRestoreServices: {ID: installRestoreServices, Title: "Restore verified service files", Destructive: true},
		"disable-phone-access": {ID: "disable-phone-access", Title: "Disable phone access and continue"},
		"reset-callback":       {ID: "reset-callback", Title: "Back up and replace callback", Destructive: true},
		"review-configuration": {ID: "review-configuration", Title: "Review configuration"},
		"reveal":               {ID: "reveal", Title: "Show affected file"},
		"download-release":     {ID: "download-release", Title: "Download current release"},
		"move-app":             {ID: "move-app", Title: "Move to Applications and reopen"},
	}
	return values[id]
}

func installDisplayText(value string, limit int, multiline bool) string {
	var output strings.Builder
	count := 0
	for _, r := range value {
		if count >= limit {
			output.WriteRune('…')
			break
		}
		if unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\t')) {
			r = ' '
		}
		output.WriteRune(r)
		count++
	}
	return output.String()
}

func (report *installPreflightReport) add(id, title, detail, path string, actions ...string) int {
	if len(report.Issues) >= 24 {
		return -1
	}
	for i, issue := range report.Issues {
		if issue.ID == id && issue.Path == path && issue.Title == title {
			return i
		}
	}
	for _, issue := range report.Issues {
		if issue.ID == id {
			sum := sha256.Sum256([]byte(title + "\x00" + path))
			id += "-" + hex.EncodeToString(sum[:4])
			break
		}
	}
	for i, issue := range report.Issues {
		if issue.ID == id {
			return i
		}
	}
	if !utf8.ValidString(path) || strings.IndexFunc(path, unicode.IsControl) >= 0 {
		path = ""
		filtered := actions[:0]
		for _, action := range actions {
			if action != "reveal" {
				filtered = append(filtered, action)
			}
		}
		actions = filtered
	}
	issue := installPreflightIssue{ID: id, Title: installDisplayText(title, 200, false), Detail: installDisplayText(detail, 1200, true), Path: path, Actions: []installRecoveryAction{}}
	for _, id := range actions {
		issue.Actions = append(issue.Actions, installAction(id))
	}
	report.Issues = append(report.Issues, issue)
	return len(report.Issues) - 1
}

// A preflight must not create coordination files or normalize permissions.
// Probe only an existing descriptor, preserving the kernel's ownership boundary.
func installInspectLock(directory, name string) (bool, error) {
	parent, err := integrationParent(filepath.Join(directory, name), false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return false, err
	}
	if uint32(info.Mode)&unix.S_IFMT != unix.S_IFREG || info.Uid != uint32(os.Getuid()) || info.Nlink != 1 || uint32(info.Mode)&0077 != 0 {
		return false, errors.New("Unsafe installer coordination lock; inspect ownership, permissions and file type")
	}
	err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, unix.Flock(fd, unix.LOCK_UN)
}

func installInspectDirectory(directory string) error {
	parent, err := integrationParent(filepath.Join(directory, ".preflight-probe"), false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return parent.Close()
}

func (install *installer) inspectService(ctx context.Context, label string) ([]byte, bool, error) {
	output, err := install.run(ctx, "launchctl", "print", install.domain()+"/"+label)
	if errors.Is(err, errInstallServiceNotLoaded) {
		return nil, false, nil
	}
	return output, err == nil, err
}

func (install *installer) validateLoadedServices(ctx context.Context) error {
	for _, label := range []string{"io.went.agent-heavy", "io.went.agent-resources"} {
		output, loaded, err := install.inspectService(ctx, label)
		if err != nil {
			return fmt.Errorf("Cannot inspect background service ownership: %w", err)
		}
		if !loaded {
			continue
		}
		definition, err := updatePlist(filepath.Join(install.launch, label+".plist"))
		if err != nil {
			return err
		}
		arguments, err := stringsFrom(definition["ProgramArguments"])
		if err != nil {
			return err
		}
		if !installLoadedServiceMatches(output, arguments) {
			return errors.New("Loaded background service differs from its owned definition; no service was changed")
		}
	}
	return nil
}

func (install *installer) legacyInstallation(node *yaml.Node) bool {
	if install.embeddedDefinition() {
		return false
	}
	if exists, _ := updateExists(filepath.Join(install.root, "agent_heavy.py")); exists {
		return true
	}
	for _, name := range []string{"state.json", "state.json.gz"} {
		if parent, err := integrationParent(filepath.Join(install.state, "pueue", name), false); err == nil {
			var info unix.Stat_t
			err = unix.Fstatat(int(parent.Fd()), name, &info, unix.AT_SYMLINK_NOFOLLOW)
			parent.Close()
			if err == nil {
				return true
			}
		}
	}
	if definition, err := updatePlist(filepath.Join(install.launch, "io.went.agent-heavy.plist")); err == nil {
		if arguments, err := stringsFrom(definition["ProgramArguments"]); err == nil && len(arguments) > 0 && filepath.Base(arguments[0]) == "pueued" {
			return true
		}
	}
	return node != nil
}

func (install *installer) installedExecutableReady() (bool, error) {
	filename := filepath.Join(install.root, "bin/agent-heavy")
	parent, err := integrationParent(filename, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), "agent-heavy", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return false, err
	}
	if uint32(info.Mode)&unix.S_IFMT != unix.S_IFREG || info.Uid != uint32(os.Getuid()) || info.Nlink != 1 || uint32(info.Mode)&0022 != 0 {
		return false, errors.New("Unsafe installed executable ownership, permissions or file type")
	}
	return uint32(info.Mode)&0100 != 0, nil
}

func (install *installer) inspectQueueReadiness(ctx context.Context) error {
	deadline := time.Now().Add(15 * time.Second)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	gate := &Gate{State: install.state, ControlDeadline: deadline}
	_, err := gate.Snapshot()
	return err
}

func (install *installer) preflight(ctx context.Context) installPreflightReport {
	report := installPreflightReport{SchemaVersion: 1, Issues: []installPreflightIssue{}}
	if !install.testing && runtime.GOOS != "darwin" {
		report.add("platform-unsupported", "macOS is required", "Agent Queue installation requires macOS.", "")
		return report
	}
	if err := install.validatePayload(); err != nil {
		actions := []string{"download-release", "reveal"}
		if install.appBundle && installValidateAppPath(install.home, install.app) != nil {
			actions = []string{"move-app", "reveal"}
		}
		report.add("payload-invalid", "Review the application package", err.Error(), install.app, actions...)
	}
	privateSafe := true
	for _, directory := range []string{install.root, filepath.Join(install.root, "bin"), install.state, filepath.Join(install.state, "pueue"), install.config, install.launch, filepath.Join(install.home, ".local/bin")} {
		if err := installInspectDirectory(directory); err != nil {
			report.add("private-path-unsafe", "Review private installation permissions", err.Error()+" No permissions or ownership were changed.", directory, "reveal")
			privateSafe = false
		}
	}
	// Dependent inspections must not read through an unsafe ancestor.
	if !privateSafe {
		return report
	}
	for _, entry := range []struct{ directory, name, id, title string }{
		{install.state, "install.lock", "installer-busy", "Another installation is running"},
		{install.state, "scheduling.lock", "scheduling-busy", "A queue change is running"},
		{filepath.Join(install.state, "remote"), "control.lock", "remote-busy", "Phone access settings are changing"},
	} {
		busy, err := installInspectLock(entry.directory, entry.name)
		if err != nil {
			report.add("private-path-unsafe", "Review installation coordination", err.Error(), filepath.Join(entry.directory, entry.name), "reveal")
		} else if busy {
			report.add(entry.id, entry.title, "Wait for the current operation to finish. Setup will recheck before making changes.", "", "wait")
		}
	}
	var node *yaml.Node
	configPath := filepath.Join(install.config, "pueue.yml")
	if exists, err := updateExists(configPath); err != nil {
		report.add("configuration-invalid", "Review queue configuration", err.Error(), configPath, "reveal")
	} else if exists {
		raw, readErr := updateReadOwned(configPath, 4*1024*1024)
		if readErr != nil {
			err = readErr
		} else {
			node, err = installDecodeConfig(raw)
		}
		if err != nil {
			report.add("configuration-invalid", "Review queue configuration", err.Error()+" Restore a verified original or backup; setup will not replace it with defaults.", configPath, "review-configuration", "reveal")
		} else {
			hash := sha256.Sum256(raw)
			report.Revision = hex.EncodeToString(hash[:])
		}
	}
	missingDefinitions := map[string]bool{}
	missingServices, missingDefinitionErr := install.missingServiceDefinitions(ctx)
	if missingDefinitionErr == nil && len(missingServices) > 0 {
		for _, service := range missingServices {
			missingDefinitions[service.label] = true
		}
		report.add("service-definitions-missing", "Restore missing background service files", "Background services are still running, but their LaunchAgent files are missing. Restore standard definitions from verified loaded commands and environment. Lost custom service options cannot be recovered. Running services and jobs will not be restarted or stopped.", install.launch, "reveal", installRestoreServices)
	}
	servicesSafe := true
	if err := install.validateServices(); err != nil {
		servicesSafe = false
		report.add("service-configuration", "Review background service configuration", err.Error()+" Custom settings require an explicit reviewed repair.", install.launch, "review-configuration", "reveal")
	}
	if servicesSafe && len(missingDefinitions) > 0 {
		servicesSafe = false // Restore the verified missing definitions before dependent recovery.
	}
	if servicesSafe {
		if err := install.validateLoadedServices(ctx); err != nil {
			servicesSafe = false
			detail := err.Error()
			if errors.Is(err, os.ErrNotExist) && missingDefinitionErr != nil {
				detail = "A running service's LaunchAgent file is missing. Automatic restoration is unavailable: " + missingDefinitionErr.Error()
			}
			report.add("service-configuration", "Review loaded background service ownership", detail, install.launch, "reveal")
		}
	}
	legacy := install.legacyInstallation(node)
	callbackSafe := false
	if node != nil {
		callback, err := installConfigCallback(node)
		callbackSafe = err == nil && install.standardCleanupCallback(callback)
	}
	if legacy && node == nil {
		report.add("configuration-missing", "Restore the legacy queue configuration", "Retained legacy queue state or its service definition exists without a valid configuration. Restore a verified original or backup before migrating; setup will not guess defaults.", configPath, "review-configuration", "reveal")
	}
	if legacy && node != nil {
		callback, err := installConfigCallback(node)
		if err != nil {
			report.add("configuration-invalid", "Review queue cleanup configuration", err.Error(), configPath, "review-configuration", "reveal")
		} else if !install.standardCleanupCallback(callback) {
			replacement := install.recoveryCallback()
			detail := "The current custom callback is preserved in the configuration file. Review that file before replacing it.\n- Current custom hook (open the file to inspect)\n+ " + replacement + "\nThe original configuration will be backed up privately. Its custom hook will no longer run. Replacement is allowed only after jobs and cleanup have finished."
			report.add("custom-callback", "Review the custom cleanup callback", detail, configPath, "review-configuration", "reveal", "reset-callback")
		}
	}
	var legacyTasks Object
	if legacy && node != nil {
		output, loaded, err := install.inspectService(ctx, "io.went.agent-heavy")
		if err != nil {
			report.add("service-configuration", "Cannot inspect the old queue service", err.Error(), install.launch, "reveal")
		} else {
			if loaded && servicesSafe {
				if err := install.matchQueueService(output); err != nil {
					report.add("service-configuration", "Review loaded queue ownership", err.Error(), install.launch, "reveal")
					servicesSafe = false
				}
			}
			if !loaded || servicesSafe {
				tasks, err := install.tasks(ctx, node, loaded)
				if err != nil {
					report.add("configuration-invalid", "Cannot inspect legacy jobs", err.Error(), configPath, "review-configuration", "reveal")
				} else {
					legacyTasks = tasks
					install.preflightJobs(&report, tasks, loaded && callbackSafe)
				}
			}
		}
	}
	if legacy {
		if err := install.requireLegacyCleanup(); err != nil {
			path := filepath.Join(install.state, "receipts")
			var pending *installCleanupPendingError
			if errors.As(err, &pending) {
				path = pending.path
			}
			detail := err.Error() + " Review jobs in the existing queue; completed jobs can use verified cleanup when offered. Do not delete lease or receipt files."
			actions := []string{"review-jobs", "reveal", "wait"}
			// Native file navigation is restricted to the account or app folder.
			// A configured external lease can be reported without offering an
			// inert button or widening that boundary.
			if path != install.home && !strings.HasPrefix(path, filepath.Clean(install.home)+string(filepath.Separator)) {
				actions = []string{"review-jobs", "wait"}
				detail += " This lease is outside your account folder. Inspect the displayed path manually; setup cannot open it automatically."
			}
			report.add("pending-cleanup", "Wait for queue work and cleanup", detail, path, actions...)
			install.preflightCleanupJobs(&report, callbackSafe, legacyTasks)
		}
		if err := install.validateRetirement(); err != nil {
			report.add("private-path-unsafe", "Review old runtime files", err.Error(), install.root, "reveal")
		}
	}
	if enabled, err := install.remoteEnabled(); err != nil {
		report.add("configuration-invalid", "Review phone access configuration", err.Error(), filepath.Join(install.config, "remote.json"), "reveal")
	} else {
		_, loaded, inspectErr := install.inspectService(ctx, "io.went.agent-remote")
		if inspectErr != nil {
			report.add("service-configuration", "Cannot inspect phone access service", inspectErr.Error(), install.launch, "reveal")
		} else if loaded && missingDefinitions["io.went.agent-remote"] {
			// The explicit service-file recovery must finish before phone recovery.
		} else if loaded && install.validatePhoneAccessRecovery(ctx) != nil {
			report.add("service-configuration", "Review loaded phone access ownership", "The loaded phone access service does not match a safe canonical definition. Setup will not stop it or replace its configuration.", filepath.Join(install.launch, "io.went.agent-remote.plist"), "reveal")
		} else if legacy && (enabled || loaded) {
			report.add("legacy-phone-access", "Phone access must be disabled", "Credentials, configuration and trusted certificates will be preserved. You can enable phone access in settings after setup.", "", "disable-phone-access")
		} else if !legacy && enabled {
			if exists, _ := updateExists(filepath.Join(install.state, "remote/error.json")); exists {
				report.add("remote-migration", "Phone access needs attention", "Review phone service diagnostics, or disable phone access and finish upgrading. Credentials and trusted certificates will be preserved.", filepath.Join(install.state, "remote/error.json"), "reveal", "disable-phone-access")
			}
		}
	}
	if install.embeddedDefinition() && servicesSafe {
		executableReady, executableErr := install.installedExecutableReady()
		_, loaded, err := install.inspectService(ctx, "io.went.agent-heavy")
		if executableErr != nil {
			report.add("private-path-unsafe", "Review the installed backend", executableErr.Error(), filepath.Join(install.root, "bin/agent-heavy"), "reveal")
		} else if !executableReady {
			// Normal installation repairs missing or non-executable components.
		} else if err != nil {
			report.add("service-configuration", "Cannot inspect queue service", err.Error(), install.launch, "reveal")
		} else if !loaded {
			report.add("service-not-ready", "The queue service is stopped", "Start the verified installed service before continuing. Existing jobs will not be restarted automatically.", filepath.Join(install.state, "daemon.log"), "reveal", "repair-service")
		} else if err := install.inspectQueueReadiness(ctx); err != nil {
			report.add("service-not-ready", "The queue service is not ready", "Inspect its startup diagnostics. Setup will not restart a running service or uncertain job attempts.", filepath.Join(install.state, "daemon.log"), "reveal", "wait")
		}
	}
	if legacy && len(missingDefinitions) == 0 {
		install.preflightLegacyProcesses(ctx, &report, servicesSafe)
	}
	return report
}

func installLoadedPID(output []byte) (int, bool) {
	pid := 0
	count := 0
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "pid = ") {
			count++
			value, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "pid = ")))
			if err != nil || value <= 1 {
				return 0, false
			}
			pid = value
		}
	}
	return pid, count == 1
}

func (install *installer) preflightLegacyProcesses(ctx context.Context, report *installPreflightReport, servicesSafe bool) {
	owned := map[int]bool{}
	resourcesLoaded := false
	for _, label := range []string{"io.went.agent-heavy", "io.went.agent-resources", "io.went.agent-remote"} {
		output, loaded, err := install.inspectService(ctx, label)
		if err != nil {
			report.add("service-configuration", "Cannot establish background process ownership", err.Error(), filepath.Join(install.launch, label+".plist"), "reveal")
			continue
		}
		if label == "io.went.agent-resources" {
			resourcesLoaded = loaded
		}
		if !loaded {
			continue
		}
		if label == "io.went.agent-remote" {
			if err := install.validatePhoneAccessRecovery(ctx); err != nil {
				report.add("service-configuration", "Review phone service ownership", err.Error(), filepath.Join(install.launch, label+".plist"), "reveal")
				continue
			}
		} else if !servicesSafe {
			continue
		}
		definition, err := updatePlist(filepath.Join(install.launch, label+".plist"))
		if err != nil {
			continue
		}
		arguments, err := stringsFrom(definition["ProgramArguments"])
		if err != nil || !installLoadedServiceMatches(output, arguments) {
			continue
		}
		if pid, ok := installLoadedPID(output); ok {
			owned[pid] = true
		}
	}
	busy, err := installInspectLock(install.state, "resource-controller.lock")
	if err != nil {
		report.add("private-path-unsafe", "Review resource controller ownership", err.Error(), filepath.Join(install.state, "resource-controller.lock"), "reveal")
	} else if busy && !resourcesLoaded {
		report.add("controller-stopping", "The old resource controller is still stopping", "Wait for its lifetime lock to be released. Setup will not remove the lock or force-stop its holder.", "", "wait")
	}
	output, err := install.run(ctx, "ps", "-axo", "pid=,command=")
	if err != nil {
		report.add("legacy-processes", "Cannot inspect old queue processes", err.Error(), "", "wait")
		return
	}
	for _, line := range strings.Split(string(output), "\n") {
		legacy := strings.Contains(line, filepath.Join(install.root, ".venv/bin/python")) || strings.Contains(line, filepath.Join(install.root, "agent_heavy.py")) || strings.Contains(line, filepath.Join(install.root, "remote/control.py")) || (strings.Contains(line, "pueued") && strings.Contains(line, filepath.Join(install.config, "pueue.yml")))
		if !legacy {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || owned[pid] {
			continue
		}
		report.add("legacy-processes", "Review old queue processes", "An old queue process remains outside a verified loaded service. Finish its work and stop it in the existing installation; setup will not kill an unidentified process.", install.root, "reveal", "wait")
		return
	}
}

func (install *installer) preflightJobs(report *installPreflightReport, tasks Object, canCancel bool) {
	jobs := []installRecoveryJob{}
	for key, raw := range tasks {
		id, err := strconv.Atoi(key)
		status, _, statusErr := TaskStatus(obj(raw))
		if err != nil || id < 0 || statusErr != nil {
			report.add("configuration-invalid", "Cannot inspect legacy jobs", "A task has an invalid identity or status.", filepath.Join(install.config, "pueue.yml"), "reveal")
			return
		}
		if status != "Done" {
			label := text(obj(raw)["label"])
			if label == "" {
				label = "Job " + key
			}
			label = installDisplayText(label, 120, false)
			jobs = append(jobs, installRecoveryJob{ID: id, Label: label, Status: status})
		}
	}
	if len(jobs) == 0 {
		return
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	detail := fmt.Sprintf("%d legacy jobs must finish or be cancelled before migration. Paused and stashed jobs will not finish until resumed in the existing queue. Normal Go upgrades preserve active jobs.", len(jobs))
	actions := []string{"review-jobs", "wait"}
	if canCancel {
		actions = append(actions, "cancel-job")
	} else {
		detail += " Review and finish or cancel these jobs in the existing installation; setup cannot run a custom callback or control an unavailable queue."
	}
	index := report.add("legacy-jobs", "Review unfinished legacy jobs", detail, "", actions...)
	if len(jobs) > 25 {
		jobs = jobs[:25]
	}
	if index >= 0 {
		report.Issues[index].Jobs = jobs
	}
}

func (install *installer) preflightCleanupJobs(report *installPreflightReport, callbackSafe bool, tasks Object) {
	parent, err := integrationParent(filepath.Join(install.state, "receipts/.preflight-probe"), false)
	if err != nil {
		return
	}
	defer parent.Close()
	entries, err := parent.ReadDir(-1)
	if err != nil {
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for i := range report.Issues {
		if report.Issues[i].ID != "pending-cleanup" {
			continue
		}
		for _, entry := range entries {
			id, err := strconv.Atoi(strings.TrimSuffix(entry.Name(), ".json"))
			if err != nil || id < 0 || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			status, _, err := TaskStatus(obj(tasks[strconv.Itoa(id)]))
			if err != nil || status != "Done" {
				continue
			}
			if len(report.Issues[i].Jobs) >= 25 {
				break
			}
			report.Issues[i].Jobs = append(report.Issues[i].Jobs, installRecoveryJob{ID: id, Label: fmt.Sprintf("Job %d", id), Status: "Cleanup pending"})
		}
		if len(report.Issues[i].Jobs) > 0 && callbackSafe {
			report.Issues[i].Actions = append(report.Issues[i].Actions, installAction("cleanup-job"))
		}
	}
}

func (install *installer) matchQueueService(output []byte) error {
	definition, err := updatePlist(filepath.Join(install.launch, "io.went.agent-heavy.plist"))
	if err != nil {
		return err
	}
	arguments, err := stringsFrom(definition["ProgramArguments"])
	if err != nil {
		return err
	}
	if !installLoadedServiceMatches(output, arguments) {
		return errors.New("Loaded queue service differs from its owned definition; no process was stopped")
	}
	return nil
}

func (install *installer) recoveryCallback() string {
	if install.embeddedDefinition() {
		return installQuote(filepath.Join(install.root, "bin/agent-heavy")) + " cleanup {{ id }}"
	}
	return installQuote(filepath.Join(install.root, ".venv/bin/python")) + " " + installQuote(filepath.Join(install.root, "agent_heavy.py")) + " cleanup {{ id }}"
}

func (install *installer) recoveryGate(node *yaml.Node) *Gate {
	g := &Gate{Config: filepath.Join(install.config, "pueue.yml"), State: install.state, Slot: install.serviceSlot, ControlEnv: map[string]string{"HOME": install.home, "PATH": install.path, "AGENT_HEAVY_STATE": install.state, "AGENT_HEAVY_CONFIG": filepath.Join(install.config, "pueue.yml")}, ControlDeadline: time.Now().Add(20 * time.Second)}
	g.Jobs = filepath.Join(g.State, "jobs")
	g.Receipts = filepath.Join(g.State, "receipts")
	g.Cancelled = filepath.Join(g.State, "cancelled")
	g.Metadata = filepath.Join(g.State, "metadata")
	if !install.embeddedDefinition() {
		g.queueControl = func(operation string, payload Object) (Object, error) {
			if operation == "snapshot" {
				snapshot, err := install.legacySnapshot(context.Background(), node, true)
				return snapshot, err
			}
			if operation != "cancel" && operation != "kill" {
				return nil, errors.New("Unsupported legacy recovery operation")
			}
			id, ok := integer(payload["id"])
			if !ok || id < 0 {
				return nil, errors.New("Invalid recovery job identity")
			}
			tasks, err := install.tasks(context.Background(), node, true)
			if err != nil {
				return nil, err
			}
			status, _, err := TaskStatus(obj(tasks[strconv.Itoa(id)]))
			if err != nil {
				return nil, err
			}
			args := []string{"pueue", "--config", g.Config, "--color", "never"}
			switch status {
			case "Queued":
				args = append(args, "stash", strconv.Itoa(id))
			case "Stashed":
				args = append(args, "remove", strconv.Itoa(id))
			case "Running", "Paused":
				args = append(args, "kill")
				if operation == "cancel" {
					args = append(args, "--signal", "SIGINT")
				}
				args = append(args, strconv.Itoa(id))
			default:
				return Object{}, nil
			}
			_, err = install.run(context.Background(), args...)
			return Object{}, err
		}
	}
	return g
}

func installValidateRecoveryFiles(g *Gate, id int) error {
	for _, directory := range []string{g.Jobs, g.Receipts, g.Cancelled, g.Metadata, g.Slot, filepath.Join(g.Slot, "agent-heavy-jobs")} {
		if err := installInspectDirectory(directory); err != nil {
			return err
		}
	}
	paths := []string{g.ReceiptPath(id), filepath.Join(g.Cancelled, strconv.Itoa(id)+".json"), filepath.Join(g.Slot, "agent-heavy.json"), filepath.Join(g.Slot, "agent-heavy-lease.json")}
	if raw, err := updateReadOwned(g.ReceiptPath(id), 32*1024*1024); err == nil {
		receipt, err := decodeObject(raw)
		if err != nil {
			return err
		}
		if token, ok := receipt["token"].(string); ok && validToken(token) {
			paths = append(paths, filepath.Join(g.Jobs, token+".json"), filepath.Join(g.Slot, "agent-heavy-jobs", fmt.Sprintf("%d-%s.json", id, token)))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, path := range paths {
		if _, err := updateReadOwned(path, 32*1024*1024); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (install *installer) recover(ctx context.Context, action string, jobID *int, revision string) error {
	if !slices.Contains([]string{"cancel-job", "cleanup-job", "repair-service", "disable-phone-access", "reset-callback", installRestoreServices}, action) {
		return errors.New("Unsupported installation recovery action")
	}
	if (action == "cancel-job" || action == "cleanup-job") != (jobID != nil) {
		return errors.New("This recovery action requires exactly one selected job")
	}
	if jobID != nil && *jobID < 0 {
		return errors.New("Recovery job ID must be nonnegative")
	}
	if err := install.validatePayload(); err != nil {
		return err
	}
	for _, directory := range []string{install.root, filepath.Join(install.root, "bin"), install.state, filepath.Join(install.state, "pueue"), install.config, install.launch} {
		if err := installInspectDirectory(directory); err != nil {
			return err
		}
	}
	// Recovery cannot operate against a nonexistent installation.
	if _, err := os.Lstat(install.state); err != nil {
		return err
	}
	lock, err := installLock(install.state, "install.lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	scheduling, err := installLock(install.state, "scheduling.lock")
	if err != nil {
		return err
	}
	defer scheduling.Close()
	if err := install.validateServices(); err != nil {
		return err
	}
	if action == installRestoreServices {
		return install.restoreServiceDefinitions(ctx)
	}
	if err := install.validateLoadedServices(ctx); err != nil {
		return err
	}
	report := install.preflight(ctx)
	allowed := false
	for _, issue := range report.Issues {
		for _, offered := range issue.Actions {
			if offered.ID == action {
				if jobID == nil {
					allowed = true
				} else {
					for _, job := range issue.Jobs {
						if job.ID == *jobID {
							allowed = true
						}
					}
				}
			}
		}
	}
	if !allowed {
		if (action == "cancel-job" || action == "cleanup-job") && !install.embeddedDefinition() {
			for _, issue := range report.Issues {
				if issue.ID == "custom-callback" {
					return errors.New("Review the custom callback before job recovery; setup will not run it implicitly")
				}
			}
		}
		return errors.New("The selected recovery is no longer offered; check the current installation again")
	}
	configPath := filepath.Join(install.config, "pueue.yml")
	var node *yaml.Node
	if exists, _ := updateExists(configPath); exists {
		node, err = installConfig(configPath)
		if err != nil {
			return err
		}
	}
	switch action {
	case "disable-phone-access":
		if err := install.validatePhoneAccessRecovery(ctx); err != nil {
			return err
		}
		_, err = install.run(ctx, filepath.Join(install.payload, "source/bin/agent-remote"), "disable")
		return err
	case "repair-service":
		if !install.embeddedDefinition() {
			return errors.New("Only an installed Go service can be started by this recovery")
		}
		if _, loaded, err := install.inspectService(ctx, "io.went.agent-heavy"); err != nil {
			return err
		} else if loaded {
			return errors.New("The service is already loaded; inspect diagnostics instead of restarting active work")
		}
		if ready, err := install.installedExecutableReady(); err != nil {
			return err
		} else if !ready {
			return errors.New("The installed backend needs payload repair before its service can start")
		}
		_, err = install.run(ctx, "launchctl", "bootstrap", install.domain(), filepath.Join(install.launch, "io.went.agent-heavy.plist"))
		return err
	case "cancel-job", "cleanup-job":
		if node == nil {
			return errors.New("Legacy job recovery requires its dedicated configuration")
		}
		callback, callbackErr := installConfigCallback(node)
		if callbackErr != nil {
			return callbackErr
		}
		if !install.standardCleanupCallback(callback) {
			return errors.New("Review the custom callback before job recovery; setup will not run it implicitly")
		}
		if err := install.validateRetirement(); err != nil {
			return err
		}
		output, loaded, err := install.inspectService(ctx, "io.went.agent-heavy")
		if err != nil {
			return err
		}
		if loaded {
			if err := install.matchQueueService(output); err != nil {
				return err
			}
		}
		if action == "cancel-job" && !loaded {
			return errors.New("Start the existing queue to cancel this job safely")
		}
		tasks, err := install.tasks(ctx, node, loaded)
		if err != nil {
			return err
		}
		task := obj(tasks[strconv.Itoa(*jobID)])
		if task == nil {
			return errors.New("The selected job no longer exists; inspect jobs again")
		}
		status, _, err := TaskStatus(task)
		if err != nil {
			return err
		}
		if action == "cleanup-job" {
			if status != "Done" {
				return errors.New("Only a completed job can be cleaned up during migration")
			}
			gate := install.recoveryGate(node)
			if err := installValidateRecoveryFiles(gate, *jobID); err != nil {
				return err
			}
			return Cleanup(gate, *jobID)
		}
		if status == "Done" {
			return errors.New("The selected job already finished; it was not cancelled")
		}
		gate := install.recoveryGate(node)
		if err := installValidateRecoveryFiles(gate, *jobID); err != nil {
			return err
		}
		return Cancel(gate, *jobID)
	case "reset-callback":
		if node == nil {
			return errors.New("Missing configuration cannot be replaced with guessed defaults")
		}
		raw, err := updateReadOwned(configPath, 4*1024*1024)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(raw)
		if revision == "" || revision != hex.EncodeToString(hash[:]) {
			return errors.New("Configuration changed since review; inspect the new proposal before applying it")
		}
		node, err := installDecodeConfig(raw)
		if err != nil {
			return err
		}
		serviceOutput, loaded, err := install.inspectService(ctx, "io.went.agent-heavy")
		if err != nil {
			return err
		}
		if loaded {
			if err := install.matchQueueService(serviceOutput); err != nil {
				return err
			}
		}
		tasks, err := install.tasks(ctx, node, loaded)
		if err != nil {
			return err
		}
		if err := installRequireDrained(tasks); err != nil {
			return err
		}
		if err := install.requireLegacyCleanup(); err != nil {
			return err
		}
		callback := installYAMLValue(installYAMLValue(node, "daemon"), "callback")
		if callback == nil || callback.Kind != yaml.ScalarNode {
			return errors.New("The callback cannot be repaired without reviewing the configuration structure")
		}
		backup := filepath.Join(install.config, "pueue.before-upgrade-"+revision+".yml")
		if existing, err := updateReadOwned(backup, 4*1024*1024); err == nil {
			if !bytes.Equal(existing, raw) {
				return errors.New("The retained configuration backup has changed")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		} else {
			parent, err := integrationParent(backup, false)
			if err != nil {
				return err
			}
			err = integrationPrivateWrite(parent, filepath.Base(backup), raw)
			if err == nil {
				err = parent.Sync()
			}
			parent.Close()
			if err != nil {
				return fmt.Errorf("Original configuration backup could not be created exclusively: %w", err)
			}
		}
		current, err := updateReadOwned(configPath, 4*1024*1024)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, raw) {
			return errors.New("Configuration changed during backup; original retained and no replacement applied")
		}
		callback.Value = install.recoveryCallback()
		callback.Tag = "!!str"
		replacement, err := yaml.Marshal(node)
		if err != nil {
			return err
		}
		return installAtomic(configPath, replacement, 0600)
	}
	return errors.New("Unsupported installation recovery action")
}

func installWritePreflight(report installPreflightReport) error {
	report.compact()
	raw, err := json.Marshal(report)
	if err != nil {
		return err
	}
	if len(raw) > 32*1024 {
		return errors.New("Preflight report exceeds its safe display limit; inspect installation diagnostics")
	}
	_, err = fmt.Fprintln(os.Stdout, string(raw))
	return err
}

// Keep every blocker represented even when long private paths or Unicode job
// labels would otherwise exceed the native transport's finite report budget.
func (report *installPreflightReport) compact() {
	raw, err := json.Marshal(report)
	if err != nil || len(raw) <= 30*1024 {
		return
	}
	for i := range report.Issues {
		issue := &report.Issues[i]
		issue.Detail = installDisplayText(issue.Detail, 200, true)
		if len(issue.Jobs) > 10 {
			issue.Jobs = issue.Jobs[:10]
		}
		for j := range issue.Jobs {
			issue.Jobs[j].Label = installDisplayText(issue.Jobs[j].Label, 40, false)
		}
	}
	raw, err = json.Marshal(report)
	if err != nil || len(raw) <= 30*1024 {
		return
	}
	for i := range report.Issues {
		issue := &report.Issues[i]
		if len(issue.Path) > 128 {
			issue.Path = ""
			actions := []installRecoveryAction{}
			for _, action := range issue.Actions {
				if action.ID != "reveal" {
					actions = append(actions, action)
				}
			}
			issue.Actions = actions
			issue.Detail = installDisplayText(issue.Detail, 140, true) + " Open installation settings to inspect its full private path."
		}
	}
}
