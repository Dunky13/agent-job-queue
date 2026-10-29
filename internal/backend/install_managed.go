package backend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
	"howett.net/plist"
)

type managedPlan struct {
	previous              *appservices.Installation
	statuses              map[string]string
	legacy                map[string]Object
	loaded                map[string]bool
	orphanedMenu          bool
	node                  *yaml.Node
	python, pueue, remote bool
	remoteCleanup         bool
	live                  *managedLiveUpgrade
}

func (install *installer) managedStatus(ctx context.Context, app, service string) (string, error) {
	args, err := appservices.Arguments(app, "status", service)
	if err != nil {
		return "", err
	}
	raw, err := install.run(ctx, args...)
	if err != nil {
		return "", err
	}
	status, err := appservices.DecodeStatus(raw, service)
	return status.Status, err
}

func (install *installer) managedAction(ctx context.Context, app, action, service string) error {
	status, err := install.managedStatus(ctx, app, service)
	if err != nil {
		return err
	}
	if status == "requiresApproval" && !(action == "unregister" && install.unregisterServices) {
		if action == "register" {
			return &installApprovalError{service: service}
		}
		return fmt.Errorf("The %s service requires approval in System Settings; approve it before retrying setup", service)
	}
	if status == "notFound" && action != "register" {
		return fmt.Errorf("The %s service definition was not found; setup is incomplete", service)
	}
	if status == "notFound" {
		// This is a framework lookup result, not proof that a bundled plist is
		// missing. Verify the actual signed package before one explicit attempt.
		if err := install.managedVerify(ctx, app); err != nil {
			return err
		}
	}
	if action == "register" && status == "enabled" || action == "unregister" && status == "notRegistered" {
		return nil
	}
	args, err := appservices.Arguments(app, action, service)
	if err != nil {
		return err
	}
	// The native bridge waits for asynchronous unregistration completion.
	if _, err := install.run(ctx, args...); err != nil {
		// The real native bridge exits 2 for a registered service awaiting
		// consent. Confirm that precise state with a read-only query before
		// treating a failed command as a resumable approval pause.
		if action == "register" {
			if current, readErr := install.managedStatus(ctx, app, service); readErr == nil && current == "requiresApproval" {
				return &installApprovalError{service: service}
			}
		}
		return err
	}
	status, err = install.managedStatus(ctx, app, service)
	if err != nil {
		return err
	}
	expected := "enabled"
	if action == "unregister" {
		expected = "notRegistered"
	}
	if status != expected {
		if action == "register" && status == "requiresApproval" {
			return &installApprovalError{service: service}
		}
		return fmt.Errorf("The %s service is %s after %s; setup is incomplete", service, status, action)
	}
	return nil
}

func (install *installer) managedVerify(ctx context.Context, app string) error {
	if err := installValidatePayloadTree(app, true); err != nil {
		return err
	}
	if err := appservices.ValidateBundle(app); err != nil {
		return err
	}
	for _, target := range []string{filepath.Join(app, "Contents/MacOS/agent-heavy"), filepath.Join(app, "Contents/MacOS/agent-remote"), app} {
		if _, err := install.run(ctx, "/usr/bin/codesign", "--verify", "--strict", "--verbose=2", target); err != nil {
			return fmt.Errorf("Managed bundle signature verification failed: %w", err)
		}
	}
	return nil
}

// Permit only settings whose behavior the static bundle definitions retain.
// Unknown legacy options cannot be silently discarded during migration.
func (install *installer) managedLegacyDefinition(label string, value Object) error {
	menu := label == "io.went.agent-queue"
	expected := Object{"Label": label, "RunAtLoad": true, "ThrottleInterval": uint64(10), "Umask": uint64(077), "WorkingDirectory": install.state}
	log := "resources.log"
	if label == "io.went.agent-heavy" {
		log = "daemon.log"
		expected["KeepAlive"] = Object{"SuccessfulExit": false}
	} else {
		expected["KeepAlive"] = true
	}
	expected["StandardErrorPath"] = filepath.Join(install.state, log)
	expected["StandardOutPath"] = filepath.Join(install.state, log)
	if menu {
		expected = Object{"Label": label, "RunAtLoad": true, "KeepAlive": false}
	}
	for key, raw := range value {
		if key == "ProgramArguments" || key == "Program" {
			continue
		}
		if key == "EnvironmentVariables" && !menu {
			env := obj(raw)
			if env == nil {
				return errors.New("Invalid legacy service environment")
			}
			allowed := install.serviceEnvironment(nil)
			allowed["PATH"] = ToolPath
			for name, field := range env {
				if !reflect.DeepEqual(field, allowed[name]) {
					if install.automaticSetup {
						continue // The verified legacy definition is backed up and retired after drain.
					}
					return fmt.Errorf("Legacy service environment %s cannot be retained by the managed bundle", name)
				}
			}
			continue
		}
		if !managedValuesEqual(raw, expected[key]) {
			if install.automaticSetup {
				continue
			}
			return fmt.Errorf("Legacy service option %s is customized; review it before managed migration", key)
		}
	}
	if menu {
		args, err := stringsFrom(value["ProgramArguments"])
		if err != nil || len(args) != 3 || args[0] != "/usr/bin/open" || args[1] != "-a" || installValidateAppPath(install.home, args[2]) != nil {
			return errors.New("Legacy menu service is customized")
		}
		// An explicit Finder target can differ from the old canonical app. The
		// old menu command remains owned; never reinterpret it as the new target.
		if args[2] != install.app && args[2] != filepath.Join(install.home, "Applications/Agent Queue.app") && args[2] != "/Applications/Agent Queue.app" {
			raw, err := installReadPayload(args[2], filepath.Join(args[2], "Contents/Info.plist"), 16384, true)
			if err != nil {
				return err
			}
			var info Object
			if _, err := plist.Unmarshal(raw, &info); err != nil || info["CFBundleIdentifier"] != "io.went.agent-queue" {
				return errors.New("Legacy menu targets another application")
			}
		}
		if program, exists := value["Program"]; exists && program != args[0] {
			return errors.New("Legacy menu service overrides its executable")
		}
	}
	return nil
}

func managedValuesEqual(left, right any) bool {
	a, err := json.Marshal(left)
	if err != nil {
		return false
	}
	b, err := json.Marshal(right)
	return err == nil && string(a) == string(b)
}

func (install *installer) managedSnapshot(ctx context.Context) (Object, error) {
	if install.queueSnapshot != nil {
		return install.queueSnapshot(ctx)
	}
	deadline := time.Now().Add(15 * time.Second)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	gate := &Gate{State: install.state, ControlDeadline: deadline}
	return gate.Snapshot()
}

func (install *installer) managedDrain(ctx context.Context, p *managedPlan) error {
	for name, label := range map[string]string{"queue-daemon.lock": "io.went.agent-heavy", "resource-controller.lock": "io.went.agent-resources"} {
		busy, err := installInspectLock(install.state, name)
		if err != nil {
			return err
		}
		if busy && !p.loaded[label] {
			return errors.New("An unowned queue process holds its lifetime lock; setup refused before service mutation")
		}
	}
	busy, err := installInspectLock(filepath.Join(install.state, "remote"), "supervisor.lock")
	if err != nil {
		return err
	}
	if busy && !p.loaded["io.went.agent-remote"] {
		return errors.New("An unowned phone access process holds its lifetime lock; setup refused before service mutation")
	}
	if (install.prepareReplacement || install.unregisterServices) && !p.loaded["io.went.agent-remote"] {
		cleanup, err := install.disabledRemoteCleanup(ctx)
		if err != nil {
			return err
		}
		if cleanup {
			return errors.New("Phone access has retained process ownership records without an owned loaded service; disable phone access before retrying")
		}
	}
	if p.pueue {
		return nil
	} // queueMigration already applies the Pueue drain and cleanup gates.
	if live, err := install.resumeManagedLive(ctx, p); err != nil {
		return err
	} else if live != nil {
		p.live = live
		return nil
	}
	exists, err := updateExists(filepath.Join(install.state, "queue.sqlite3"))
	if err != nil {
		return err
	}
	if !p.loaded["io.went.agent-heavy"] && !exists {
		return nil
	}
	if !p.loaded["io.went.agent-heavy"] {
		return install.managedOfflineDrain(ctx)
	}
	snapshot, err := install.managedSnapshot(ctx)
	if err != nil {
		// A registered daemon that launchd cannot spawn, such as a replaced
		// bundle awaiting re-registration, answers nothing and holds no
		// lifetime lock. Its stopped store is then authoritative.
		if busy, lockErr := installInspectLock(install.state, "queue-daemon.lock"); lockErr == nil && !busy && exists {
			return install.managedOfflineDrain(ctx)
		}
		return fmt.Errorf("Cannot verify the installed queue is drained; no service was changed: %w", err)
	}
	tasks := obj(snapshot["tasks"])
	if tasks == nil {
		return errors.New("Cannot verify drained queue: invalid queue snapshot")
	}
	if err := installRequireDrained(tasks); err != nil {
		var drain *installDrainPendingError
		if errors.As(err, &drain) {
			live, liveErr := install.planManagedLive(ctx, p)
			if liveErr != nil {
				return liveErr
			}
			if live != nil {
				p.live = live
				return nil
			}
		}
		return err
	}
	// A command can have a durable terminal result while its original caller
	// is still reading output. Its executable requires the same retained bundle.
	if live, err := install.planManagedLive(ctx, p); err != nil {
		return err
	} else if live != nil {
		p.live = live
		return nil
	}
	return install.requireLegacyCleanup()
}

// Inspect a stopped queue using private copies. SQLite may create sidecars even
// in read-only mode, so never open the user's live database during preflight.
func (install *installer) managedOfflineDrain(ctx context.Context) error {
	for _, name := range []string{"queue-daemon.lock", "resource-controller.lock"} {
		busy, err := installInspectLock(install.state, name)
		if err != nil {
			return err
		}
		if busy {
			return errors.New("An unowned or stopping queue process still holds its lifetime lock; setup refused")
		}
	}
	path := QueueDatabasePath(install.state)
	if err := queuePrivateFiles(path); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp("", "agent-queue-install-inspect-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		raw, err := appservices.ReadFile(install.home, path+suffix, updateMaxArchive, false)
		if errors.Is(err, os.ErrNotExist) && suffix != "" {
			continue
		}
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(temporary, "queue.sqlite3"+suffix), raw, 0600); err != nil {
			return err
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(temporary, "queue.sqlite3"))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var version, unfinished int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 1 && version != 2 {
		return errors.New("Unsupported stopped queue schema")
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM queue_jobs WHERE phase != 'done'").Scan(&unfinished); err != nil {
		return err
	}
	if unfinished != 0 {
		return errors.New("Managed setup requires a drained queue; stopped queue has unfinished jobs or attempts")
	}
	store := &queueStore{db: db, path: filepath.Join(temporary, "queue.sqlite3")}
	snapshot, err := store.snapshot()
	if err != nil {
		return err
	}
	if err := installRequireDrained(obj(snapshot["tasks"])); err != nil {
		return err
	}
	return install.requireLegacyCleanup()
}

func (install *installer) planManaged(ctx context.Context) (*managedPlan, error) {
	p := &managedPlan{statuses: map[string]string{}, legacy: map[string]Object{}, loaded: map[string]bool{}}
	if _, err := install.managedWrappers(); err != nil {
		return nil, err
	}
	var err error
	p.previous, err = appservices.Load(install.home)
	if err != nil {
		return nil, err
	}
	if stale, err := install.managedStaleInstallation(ctx, p.previous); err != nil {
		return nil, err
	} else if stale {
		// The managed runtime was installed before, so legacy migrations stay
		// complete. Setup adopts this bundle and rewrites the location record.
		p.previous = &appservices.Installation{SchemaVersion: 1, App: install.app, ServiceManagement: true}
	}
	if p.previous != nil {
		if p.previous.App != install.app {
			return nil, fmt.Errorf("Agent Queue is already installed at %s; that location changed. Quit Agent Queue and drag this copy over that one, or move that copy to the Trash, then open Agent Queue again", p.previous.App)
		}
		if err := appservices.ValidateApp(install.home, p.previous.App); err != nil {
			return nil, err
		}
	}
	if err := install.managedPublicationAccess(); err != nil {
		return nil, err
	}
	if exists, err := updateExists(filepath.Join(install.config, "pueue.yml")); err != nil {
		return nil, err
	} else if exists {
		p.node, err = installConfig(filepath.Join(install.config, "pueue.yml"))
		if err != nil {
			return nil, err
		}
		callback, err := installConfigCallback(p.node)
		if err != nil {
			return nil, err
		}
		if !install.standardCleanupCallback(callback) {
			if !install.automaticSetup || p.previous == nil {
				return nil, errInstallCustomCallback
			}
			// The managed Go runtime never executes Pueue callbacks. Preserve
			// an obsolete customized config byte-for-byte instead of treating
			// it as a legacy service or rewriting inactive operator settings.
			p.node = nil
		}
	}
	if err := install.validateServices(); err != nil {
		return nil, err
	}
	for _, label := range []string{"io.went.agent-heavy", "io.went.agent-resources", "io.went.agent-queue"} {
		filename := filepath.Join(install.launch, label+".plist")
		if exists, err := updateExists(filename); err != nil {
			return nil, err
		} else if exists {
			definition, err := updatePlist(filename)
			if err != nil {
				return nil, err
			}
			if err := install.managedLegacyDefinition(label, definition); err != nil {
				return nil, err
			}
			p.legacy[label] = definition
		}
		output, loaded, err := install.inspectService(ctx, label)
		if err != nil {
			return nil, err
		}
		p.loaded[label] = loaded
		if !loaded {
			continue
		}
		if definition := p.legacy[label]; definition != nil {
			args, err := stringsFrom(definition["ProgramArguments"])
			if err != nil || !installLoadedServiceMatches(output, args) {
				return nil, errors.New("Loaded legacy service ownership differs; no process was stopped")
			}
		} else {
			if label == "io.went.agent-queue" {
				// Historical Homebrew removal deleted this plist without bootout.
				// Accept only the exact canonical old open command, including its
				// real executable. A matching label alone establishes no custody.
				if p.previous != nil {
					return nil, errors.New("Loaded menu launch service has no owned definition")
				}
				for _, app := range []string{install.app, filepath.Join(install.home, "Applications/Agent Queue.app"), "/Applications/Agent Queue.app"} {
					arguments := []string{"/usr/bin/open", "-a", app}
					if installLoadedServiceProgramMatches(output, "/usr/bin/open", arguments) {
						p.legacy[label] = Object{"Label": label, "ProgramArguments": arguments}
						p.orphanedMenu = true
						break
					}
				}
				if !p.orphanedMenu {
					return nil, errors.New("Loaded menu launch service has no owned canonical command")
				}
				continue
			}
			// A pending retry can have registered the bundle before metadata was
			// published. The signed target plus exact loaded argv establishes ownership.
			if err := install.managedLoaded(output, label, install.app); err != nil {
				return nil, err
			}
			if p.previous == nil {
				if exists, err := updateExists(filepath.Join(install.root, "install-in-progress.json")); err != nil || !exists {
					return nil, errors.New("Loaded managed service has no installation custody metadata")
				}
			}
		}
	}
	// Inspect registrations even for a first install: a Finder-copied bundle may
	// already own services. Status is read-only; denied approval is never retried.
	statusApp := install.app
	if !install.appBundle {
		if exists, err := updateExists(install.app); err != nil {
			return nil, err
		} else if !exists {
			statusApp = ""
		}
	}
	if statusApp != "" {
		raw, err := installReadPayload(statusApp, filepath.Join(statusApp, "Contents/Info.plist"), 16384, true)
		if err != nil {
			return nil, err
		}
		var info Object
		if _, err := plist.Unmarshal(raw, &info); err != nil {
			return nil, err
		}
		if info["AgentQueueServiceManagement"] == true {
			if err := install.managedVerify(ctx, statusApp); err != nil {
				return nil, err
			}
			for _, service := range []string{"queue", "resources", "remote", "login"} {
				status, err := install.managedStatus(ctx, statusApp, service)
				if err != nil {
					return nil, err
				}
				if status == "requiresApproval" && !install.unregisterServices {
					return nil, &installApprovalError{service: service}
				}
				if status == "notFound" && (install.prepareReplacement || install.unregisterServices) {
					return nil, fmt.Errorf("The %s service is %s; resolve it before retrying setup", service, status)
				}
				p.statuses[service] = status
			}
		}
	}
	if p.previous != nil {
		p.python, err = updateExists(filepath.Join(install.root, "agent_heavy.py"))
	} else {
		p.python, err = install.pythonMigration(ctx, p.node)
	}
	if err != nil {
		return nil, err
	}
	if p.python {
		if err := install.validateRetirement(); err != nil {
			return nil, err
		}
	}
	if p.previous == nil {
		p.pueue, err = install.queueMigration(ctx, p.node)
		if err != nil {
			return nil, err
		}
	}
	p.remote, err = install.remoteEnabled()
	if err != nil {
		return nil, err
	}
	legacyRemote, err := updateExists(filepath.Join(install.launch, "io.went.agent-remote.plist"))
	if err != nil {
		return nil, err
	}
	if p.previous == nil || legacyRemote {
		if err := install.validatePhoneAccessRecovery(ctx); err != nil {
			return nil, err
		}
	}
	if legacyRemote {
		definition, err := updatePlist(filepath.Join(install.launch, "io.went.agent-remote.plist"))
		if err != nil {
			return nil, err
		}
		p.legacy["io.went.agent-remote"] = definition
	}
	if output, loaded, err := install.inspectService(ctx, "io.went.agent-remote"); err != nil {
		return nil, err
	} else {
		p.loaded["io.went.agent-remote"] = loaded
		if loaded && p.previous != nil && !legacyRemote {
			if err := install.managedLoaded(output, "io.went.agent-remote", install.app); err != nil {
				return nil, err
			}
		}
	}
	if !p.remote {
		p.remoteCleanup, err = install.disabledRemoteCleanup(ctx)
		if err != nil {
			return nil, err
		}
	}
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		if _, err := appservices.ReadFile(install.home, filepath.Join(install.root, "bin", name), updateMaxArchive, false); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if err := install.managedDrain(ctx, p); err != nil {
		return nil, err
	}
	if p.previous == nil && (p.pueue || p.python) {
		report := installPreflightReport{}
		install.preflightLegacyProcesses(ctx, &report, true)
		if len(report.Issues) != 0 {
			return nil, errors.New(report.Issues[0].Detail)
		}
	}
	return p, nil
}

// Finder deletion or a move between Applications folders can leave the
// location record naming a missing app. Once no service runs from that app and
// no retained backup sits beside it, the record owns nothing.
func (install *installer) managedStaleInstallation(ctx context.Context, previous *appservices.Installation) (bool, error) {
	if !install.appBundle || install.prepareReplacement || install.unregisterServices || previous == nil || previous.App == install.app {
		return false, nil
	}
	if exists, err := updateExists(previous.App); err != nil || exists {
		return false, err
	}
	if retained, err := updateExists(filepath.Join(filepath.Dir(previous.App), ".Agent Queue.previous.app")); err != nil || retained {
		return false, err
	}
	for _, label := range []string{"io.went.agent-heavy", "io.went.agent-resources", "io.went.agent-remote"} {
		if _, loaded, err := install.inspectService(ctx, label); err != nil || loaded {
			return false, err
		}
	}
	return true, nil
}

func (install *installer) managedLoaded(raw []byte, label, app string) error {
	helper, command := "agent-heavy", "daemon"
	if label == "io.went.agent-resources" {
		command = "scheduler"
	}
	if label == "io.went.agent-remote" {
		helper, command = "agent-remote", "serve"
	}
	executable := filepath.Join(app, "Contents/MacOS", helper)
	for _, first := range []string{executable, helper, "Contents/MacOS/" + helper} {
		if installLoadedServiceProgramMatches(raw, executable, []string{first, command}) {
			return nil
		}
	}
	if managedLoadedBundleProgram(raw, executable, helper, command) {
		return nil
	}
	return errors.New("Loaded service differs from the canonical managed helper; no process was stopped")
}

// Newer launchd prints a bundle-relative "program identifier" for Service
// Management agents instead of an absolute program. A running instance
// establishes custody only through its kernel-recorded executable. Without a
// process (for example a replaced bundle awaiting re-registration) nothing can
// be stopped in error, so this app's bundle and helper identity suffice.
func managedLoadedBundleProgram(raw []byte, executable, helper, command string) bool {
	identifiers, bundles, pids, blocks := 0, 0, 0, 0
	identified, bundled := false, false
	pid := 0
	arguments := []string{}
	inArguments := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if inArguments {
			if trimmed == "}" {
				inArguments = false
			} else {
				arguments = append(arguments, trimmed)
			}
			continue
		}
		switch {
		case trimmed == "arguments = {":
			blocks++
			inArguments = true
		case strings.HasPrefix(trimmed, "program identifier = "):
			identifiers++
			value := strings.TrimPrefix(trimmed, "program identifier = ")
			if mode := strings.LastIndex(value, " (mode: "); mode >= 0 {
				value = value[:mode]
			}
			identified = value == "Contents/MacOS/"+helper
		case strings.HasPrefix(trimmed, "parent bundle identifier = "):
			bundles++
			bundled = trimmed == "parent bundle identifier = io.went.agent-queue"
		case strings.HasPrefix(line, "\tpid = "):
			pids++
			value, err := strconv.Atoi(strings.TrimPrefix(line, "\tpid = "))
			if err == nil {
				pid = value
			}
		}
	}
	if identifiers != 1 || bundles != 1 || pids > 1 || blocks != 1 || inArguments || !identified || !bundled ||
		!slices.Equal(arguments, []string{helper, command}) {
		return false
	}
	if pids == 0 {
		return true
	}
	if pid <= 1 {
		return false
	}
	path, err := installProcessExecutable(pid)
	return err == nil && path == executable
}

func (install *installer) stopManagedLegacy(ctx context.Context, p *managedPlan) error {
	if definition := p.legacy["io.went.agent-remote"]; definition != nil {
		if err := install.validatePhoneAccessRecovery(ctx); err != nil {
			return err
		}
		current, err := updatePlist(filepath.Join(install.launch, "io.went.agent-remote.plist"))
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, definition) {
			return errors.New("Phone service definition changed before shutdown")
		}
		if _, loaded, err := install.inspectService(ctx, "io.went.agent-remote"); err != nil {
			return err
		} else if loaded {
			if _, err := install.run(ctx, "launchctl", "bootout", install.domain()+"/io.went.agent-remote"); err != nil {
				return err
			}
		}
	}
	if p.pueue || p.python && !install.embeddedDefinition() {
		if err := install.stopLegacy(ctx, p.node); err != nil {
			return err
		}
	} else {
		for _, label := range []string{"io.went.agent-resources", "io.went.agent-heavy"} {
			if p.legacy[label] == nil || !p.loaded[label] {
				continue
			}
			current, err := updatePlist(filepath.Join(install.launch, label+".plist"))
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(current, p.legacy[label]) {
				return errors.New("Legacy definition changed before shutdown; no uncertain process was stopped")
			}
			output, loaded, err := install.inspectService(ctx, label)
			if err != nil {
				return err
			}
			if !loaded {
				continue
			}
			arguments, err := stringsFrom(current["ProgramArguments"])
			if err != nil || !installLoadedServiceMatches(output, arguments) {
				return errors.New("Loaded legacy ownership changed before shutdown; no uncertain process was stopped")
			}
			if _, err := install.run(ctx, "launchctl", "bootout", install.domain()+"/"+label); err != nil {
				return err
			}
		}
	}
	if p.legacy["io.went.agent-queue"] != nil && p.loaded["io.went.agent-queue"] {
		if p.orphanedMenu {
			if exists, err := updateExists(filepath.Join(install.launch, "io.went.agent-queue.plist")); err != nil {
				return err
			} else if exists {
				return errors.New("Menu definition reappeared before shutdown; review its ownership")
			}
		}
		output, loaded, err := install.inspectService(ctx, "io.went.agent-queue")
		if err != nil {
			return err
		}
		arguments, err := stringsFrom(p.legacy["io.went.agent-queue"]["ProgramArguments"])
		if err != nil || loaded && !installLoadedServiceMatches(output, arguments) {
			return errors.New("Loaded menu ownership changed before shutdown")
		}
		if _, err := install.run(ctx, "launchctl", "bootout", install.domain()+"/io.went.agent-queue"); err != nil {
			return err
		}
	}
	for label := range p.legacy {
		if _, loaded, err := install.inspectService(ctx, label); err != nil {
			return err
		} else if loaded {
			return errors.New("Legacy service is still loaded; managed registration refused")
		}
		// Re-read before removing: never unlink a definition replaced by another operator.
		filename := filepath.Join(install.launch, label+".plist")
		current, err := updatePlist(filename)
		if p.orphanedMenu && label == "io.went.agent-queue" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, p.legacy[label]) {
			return errors.New("Legacy definition changed during migration")
		}
		parent, err := integrationParent(filename, false)
		if err != nil {
			return err
		}
		err = unix.Unlinkat(int(parent.Fd()), filepath.Base(filename), 0)
		if err == nil {
			err = parent.Sync()
		}
		parent.Close()
		if err != nil {
			return err
		}
	}
	// The daemon and scheduler lifetime locks prove that detached instances have
	// finished. Taking a lock never authorizes killing an uncertain process.
	for _, name := range []string{"queue-daemon.lock", "resource-controller.lock"} {
		lock, err := installLock(install.state, name)
		if err != nil {
			return fmt.Errorf("Old service has not stopped: %w", err)
		}
		lock.Close()
	}
	return nil
}

func managedBundleDigest(app string) ([32]byte, error) {
	hash := sha256.New()
	err := filepath.WalkDir(app, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(app, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(hash, "%d:%s\x00", len(relative), relative)
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("Unsafe bundle entry")
		}
		raw, err := installReadPayload(app, path, updateMaxArchive, true)
		if err != nil {
			return err
		}
		fmt.Fprintf(hash, "%d:", len(raw))
		_, err = hash.Write(raw)
		return err
	})
	var result [32]byte
	copy(result[:], hash.Sum(nil))
	return result, err
}

// Shared Applications is a system-owned parent, never a private directory to
// chmod. Refuse a payload replacement this account cannot finish before stopping
// services; immutable root-owned bundles remain supported for explicit setup.
func (install *installer) managedPublicationAccess() error {
	parent := filepath.Dir(install.app)
	publication, err := install.managedPublicationParent(false)
	if err != nil {
		return err
	}
	defer publication.Close()
	if err := install.managedLifecycleCustody(); err != nil {
		return err
	}
	if install.appBundle {
		if err := install.managedBackupAccess(); err != nil {
			return err
		}
		if exists, err := updateExists(install.managedPreviousPath()); err != nil {
			return err
		} else if exists {
			if err := unix.Faccessat(int(publication.Fd()), ".", unix.W_OK|unix.X_OK, 0); err != nil {
				return errors.New("Completing the retained publication requires write access to its application directory; no services were changed")
			}
		}
		return nil // No publication writes are required when no backup is retained.
	}
	if install.prepareReplacement || install.unregisterServices {
		return nil // These operations preserve app bundles rather than publish them.
	}
	if err := install.managedBackupAccess(); err != nil {
		return err
	}
	if err := install.managedStagingAccess(publication); err != nil {
		return err
	}
	if err := managedCopySourceAccess(filepath.Join(install.payload, "Agent Queue.app")); err != nil {
		return err
	}
	if exists, err := updateExists(parent); err != nil {
		return err
	} else if exists {
		if err := unix.Access(parent, unix.W_OK|unix.X_OK); err != nil {
			return errors.New("Application directory is not writable by this account; no services were changed")
		}
	}
	if exists, err := updateExists(install.app); err != nil {
		return err
	} else if !exists {
		return nil
	}
	if err := installValidatePayloadTree(install.app, false); err != nil {
		return fmt.Errorf("Application cannot be replaced by this account; no services were changed: %w", err)
	}
	return filepath.WalkDir(install.app, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		access := uint32(unix.W_OK)
		if entry.IsDir() {
			access |= unix.X_OK
		}
		if err := unix.Access(path, access); err != nil {
			return errors.New("Application code is not writable by this account; no services were changed")
		}
		return nil
	})
}

// Publish a verified candidate before registration, retaining the previous
// bundle until the entire installation has become ready and published identity.
func (install *installer) publishManaged(ctx context.Context) (result error) {
	if err := install.validatePayload(); err != nil {
		return err
	}
	expected := install.identity
	if err := install.managedPublicationAccess(); err != nil {
		return err
	}
	if install.appBundle {
		before, err := managedCaptureBundle(install.app)
		if err != nil {
			return err
		}
		if err := install.managedVerify(ctx, install.app); err != nil {
			return err
		}
		if err := install.managedValidateExpected(expected); err != nil {
			return err
		}
		return managedRequireBundle(install.app, before)
	}
	parent, err := install.managedPublicationParent(true)
	if err != nil {
		return err
	}
	defer parent.Close()
	source := filepath.Join(install.payload, "Agent Queue.app")
	selected, err := managedCaptureBundle(source)
	if err != nil {
		return err
	}
	if !selected.Build.Equal(expected) {
		return errors.New("Managed source differs from the expected pending candidate")
	}
	var old *managedBundleIdentity
	if exists, err := updateExists(install.app); err != nil {
		return err
	} else if exists {
		current, err := managedCaptureBundle(install.app)
		if err != nil {
			return err
		}
		old = &current
		// The backup/receipt gate above also applies to this retry shortcut.
		if selected.Digest == current.Digest {
			if err := install.managedVerify(ctx, install.app); err != nil {
				return err
			}
			if err := install.managedValidateExpected(expected); err != nil {
				return err
			}
			if err := managedRequireBundle(source, selected); err != nil {
				return err
			}
			return managedRequireBundle(install.app, current)
		}
	}
	if exists, err := updateExists(install.managedPreviousPath()); err != nil {
		return err
	} else if exists {
		return errors.New("A previous application is retained from incomplete setup; retry that exact installer before another replacement")
	}
	staging := filepath.Join(filepath.Dir(install.app), ".Agent Queue.installing-"+token()+".app")
	if err := install.managedCopyApp(ctx, source, staging); err != nil {
		return err
	}
	// Hold the actual stage root until cleanup so inode recycling cannot turn a
	// replacement directory into one of our owned objects.
	stage, err := installPayloadParent(staging, filepath.Join(staging, ".custody-probe"), true)
	if err != nil {
		return err
	}
	defer stage.Close()
	var ownedReceipt []byte
	defer func() {
		if _, err := os.Lstat(staging); errors.Is(err, os.ErrNotExist) {
			return
		}
		if err := managedCopyNamed(parent, filepath.Base(staging), stage); err != nil {
			result = errors.Join(result, fmt.Errorf("Staged application custody changed; evidence preserved: %w", err))
			return
		}
		if old == nil && ownedReceipt != nil {
			if exists, err := updateExists(install.app); err != nil || exists {
				result = errors.Join(result, errors.New("Publication target appeared before stage cleanup; receipt and stage preserved"), err)
				return
			}
			if err := install.managedRemoveFile(install.managedReceiptPath(), ownedReceipt); err != nil {
				result = errors.Join(result, err)
				return
			}
		}
		root, err := os.OpenRoot(filepath.Dir(staging))
		if err != nil {
			result = errors.Join(result, err)
			return
		}
		result = errors.Join(result, root.RemoveAll(filepath.Base(staging)))
		root.Close()
	}()
	candidate, err := managedCaptureBundle(staging)
	if err != nil {
		return err
	}
	if candidate.Digest != selected.Digest || !candidate.Build.Equal(expected) {
		return errors.New("Managed staged candidate differs from the expected pending build")
	}
	// Source, pending identity, target ordering and exact old object are checked
	// again after all staging/signature work, immediately before either rename.
	if err := install.managedValidateExpected(expected); err != nil {
		return err
	}
	if err := managedRequireBundle(source, selected); err != nil {
		return err
	}
	if err := install.managedPublicationAccess(); err != nil {
		return err
	}
	if err := install.managedCheckPublicationParent(parent); err != nil {
		return err
	}
	if old != nil {
		if err := managedRequireBundle(install.app, *old); err != nil {
			return err
		}
	} else if exists, err := updateExists(install.app); err != nil {
		return err
	} else if exists {
		return errors.New("Managed publication target appeared during staging; evidence preserved")
	}
	if err := managedRequireBundle(staging, candidate); err != nil {
		return err
	}
	receipt := managedPublicationReceipt{SchemaVersion: 1, App: install.app, Pending: expected, Candidate: candidate, Backup: old}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	ownedReceipt = append(raw, '\n')
	if err := installAtomic(install.managedReceiptPath(), ownedReceipt, 0600); err != nil {
		ownedReceipt = nil
		return err
	}
	if old != nil {
		if err := unix.Renameat(int(parent.Fd()), filepath.Base(install.app), int(parent.Fd()), filepath.Base(install.managedPreviousPath())); err != nil {
			return err
		}
		if err := parent.Sync(); err != nil {
			return err // Receipt and last good backup remain recovery evidence.
		}
	}
	if err := unix.Renameat(int(parent.Fd()), filepath.Base(staging), int(parent.Fd()), filepath.Base(install.app)); err != nil {
		return err // Recovery uses the exact durable receipt, never a blind restore.
	}
	if err := parent.Sync(); err != nil {
		return err
	}
	return managedRequireBundle(install.app, candidate)
}

func (install *installer) managedSupport() error {
	if _, err := install.managedWrappers(); err != nil {
		return err
	}
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		if err := installAtomic(filepath.Join(install.home, ".local/bin", name), install.managedWrapper(name), 0700); err != nil {
			return err
		}
	}
	return nil
}

func (install *installer) managedWrapper(name string) []byte {
	return managedWrapperFor(install.app, name)
}

func managedWrapperFor(app, name string) []byte {
	return []byte("#!/bin/sh\nexec " + installQuote(filepath.Join(app, "Contents/MacOS", name)) + " \"$@\"\n")
}

// Exact generated content is required in addition to no-follow ownership
// checks. A private custom script is not permission to overwrite or delete it.
func (install *installer) managedWrappers() (map[string][]byte, error) {
	wrappers := map[string][]byte{}
	// A wrapper generated for the recorded location is also owned, so a Finder
	// move between Applications folders can rewrite it for the adopted bundle.
	recorded := map[string][]byte{}
	if previous, err := appservices.Load(install.home); err == nil && previous != nil && install.appBundle {
		for _, name := range []string{"agent-heavy", "agent-remote"} {
			recorded[name] = managedWrapperFor(previous.App, name)
		}
	}
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		filename := filepath.Join(install.home, ".local/bin", name)
		raw, err := appservices.ReadFile(install.home, filename, 16384, false)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		legacy := []byte("#!/bin/sh\nexec \"$HOME/.local/share/agent-heavy/bin/" + name + "\" \"$@\"\n")
		pythonModule := "agent_heavy.py"
		if name == "agent-remote" {
			pythonModule = "remote/control.py"
		}
		// Exact wrappers shipped before the Go migration (452f076's parent).
		python := []byte("#!/bin/sh\nset -eu\nroot=\"${HOME:?}/.local/share/agent-heavy\"\nexec \"$root/.venv/bin/python\" \"$root/" + pythonModule + "\" \"$@\"\n")
		if !bytes.Equal(raw, install.managedWrapper(name)) && !bytes.Equal(raw, legacy) && !bytes.Equal(raw, python) && !bytes.Equal(raw, recorded[name]) {
			return nil, fmt.Errorf("CLI wrapper %s is customized; no service was stopped", name)
		}
		wrappers[filename] = raw
	}
	return wrappers, nil
}

func (install *installer) managedRemoveFile(filename string, expected []byte) error {
	raw, err := appservices.ReadFile(install.home, filename, updateMaxArchive, false)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, expected) {
		return errors.New("Installation file changed before retirement; evidence preserved")
	}
	parent, err := integrationParent(filename, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := unix.Unlinkat(int(parent.Fd()), filepath.Base(filename), 0); err != nil {
		return err
	}
	return parent.Sync()
}

// Preserve the newest verified version evidence even when removing an older
// app after preparation of a newer candidate. Removal must not reset ordering.
func (install *installer) managedRemovalIdentity() (BuildInfo, error) {
	identity := install.identity
	for _, name := range []string{"build-info.json", "install-in-progress.json"} {
		other, err := installReadInfo(filepath.Join(install.root, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return BuildInfo{}, err
		}
		if other.Build != nil && (identity.Build == nil || *other.Build > *identity.Build) {
			identity = other
		} else if other.Build != nil && identity.Build != nil && *other.Build == *identity.Build && !other.Equal(identity) {
			return BuildInfo{}, errors.New("Conflicting tracked installation identities; removal refused")
		}
	}
	return identity, nil
}

func (install *installer) managedStopMenu(ctx context.Context, source string) error {
	exists, err := updateExists(install.app)
	if err != nil || !exists {
		return err
	}
	output, err := install.run(ctx, filepath.Join(source, "Contents/MacOS/AgentQueue"), "--prepare-app-replacement", install.app)
	if err != nil {
		return fmt.Errorf("The previous menu app could not quit; preserve its bundle and retry after quitting: %w", err)
	}
	result, err := decodeObject(output)
	schema, schemaOK := integer(result["schemaVersion"])
	if err != nil || !schemaOK || schema != 1 || result["app"] != install.app || result["status"] != "stopped" {
		return errors.New("The previous menu app did not confirm shutdown; its bundle is preserved")
	}
	return nil
}

// Preparation is an explicit mutation, separate from read-only preflight. The
// cask may move/delete its app artifact only after this operation succeeds.
func (install *installer) prepareManaged(ctx context.Context) error {
	for _, directory := range []string{install.root, install.state, install.config, install.launch, filepath.Join(install.home, ".local/bin"), filepath.Join(install.state, "remote")} {
		if err := installInspectDirectory(directory); err != nil {
			return err
		}
	}
	for _, entry := range []struct{ directory, name string }{{install.state, "install.lock"}, {install.state, "scheduling.lock"}, {filepath.Join(install.state, "remote"), "control.lock"}} {
		busy, err := installInspectLock(entry.directory, entry.name)
		if err != nil {
			return err
		}
		if busy {
			return errors.New("Another installer or queue change is running; finish it before trying again")
		}
	}
	source := install.app
	if !install.appBundle {
		source = filepath.Join(install.payload, "Agent Queue.app")
	}
	if err := install.managedVerify(ctx, source); err != nil {
		return err
	}
	if _, err := install.managedWrappers(); err != nil {
		return err
	}
	if install.unregisterServices {
		if _, err := install.managedRemovalIdentity(); err != nil {
			return err
		}
	}
	if _, err := install.planManaged(ctx); err != nil {
		return err
	}
	// The first complete inspection above creates no files. Recheck under all
	// three locks, retaining scheduling.lock through drain and daemon shutdown.
	for _, directory := range []string{install.root, install.state, filepath.Join(install.state, "remote")} {
		exists, err := updateExists(directory)
		if err != nil {
			return err
		}
		if exists {
			if err := installInspectDirectory(directory); err != nil {
				return err
			}
		} else if err := installDirectory(directory); err != nil {
			return err
		}
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
	remote, err := installLock(filepath.Join(install.state, "remote"), "control.lock")
	if err != nil {
		return err
	}
	defer remote.Close()
	if err := install.validatePayload(); err != nil {
		return err
	}
	if err := install.managedVerify(ctx, source); err != nil {
		return err
	}
	wrappers, err := install.managedWrappers()
	if err != nil {
		return err
	}
	p, err := install.planManaged(ctx)
	if err != nil {
		return err
	}
	identity := install.identity
	if install.unregisterServices {
		identity, err = install.managedRemovalIdentity()
		if err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	marker := filepath.Join(install.root, "install-in-progress.json")
	if err := installAtomic(marker, raw, 0600); err != nil {
		return err
	}
	if err := install.quiesceManaged(ctx, p); err != nil {
		return err
	}
	if err := install.managedStopMenu(ctx, source); err != nil {
		return err
	}
	for service := range p.statuses {
		status, err := install.managedStatus(ctx, install.app, service)
		if err != nil {
			return err
		}
		if status != "notRegistered" {
			return fmt.Errorf("Service %s is still registered; application must be preserved", service)
		}
	}
	if !install.unregisterServices {
		return nil
	}
	// Confirm every wrapper again before retiring any metadata. Keep private
	// queue state, credentials, configuration, history and retained apps intact.
	current, err := install.managedWrappers()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(wrappers, current) {
		return errors.New("CLI wrappers changed during service removal; evidence preserved")
	}
	if err := install.managedLifecycleCustody(); err != nil {
		return err
	}
	// Only candidate-only receipts can reach removal. Retire their private
	// custody before pending state; never delete a retained application backup.
	if err := install.managedCleanupBackup(); err != nil {
		return err
	}
	if err := installAtomic(filepath.Join(install.root, "build-info.json"), raw, 0600); err != nil {
		return err
	}
	for filename, expected := range wrappers {
		if err := install.managedRemoveFile(filename, expected); err != nil {
			return err
		}
	}
	if p.previous != nil {
		metadata, err := appservices.Load(install.home)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(metadata, p.previous) {
			return errors.New("Managed installation custody changed during removal")
		}
		filename := filepath.Join(install.root, "installation.json")
		expected, err := appservices.ReadFile(install.home, filename, 16384, false)
		if err != nil {
			return err
		}
		if err := install.managedRemoveFile(filename, expected); err != nil {
			return err
		}
	}
	return install.managedRemoveFile(marker, raw)
}

func (install *installer) managedStopped(ctx context.Context) error {
	for _, label := range []string{"io.went.agent-heavy", "io.went.agent-resources", "io.went.agent-remote", "io.went.agent-queue"} {
		if _, loaded, err := install.inspectService(ctx, label); err != nil {
			return err
		} else if loaded {
			return errors.New("Service is still loaded; application must be preserved")
		}
	}
	for _, entry := range []struct{ directory, name string }{{install.state, "queue-daemon.lock"}, {install.state, "resource-controller.lock"}, {filepath.Join(install.state, "remote"), "supervisor.lock"}} {
		busy, err := installInspectLock(entry.directory, entry.name)
		if err != nil {
			return err
		}
		if busy {
			return errors.New("Service still holds its lifetime lock; application must be preserved")
		}
	}
	if install.prepareReplacement || install.unregisterServices {
		cleanup, err := install.disabledRemoteCleanup(ctx)
		if err != nil {
			return err
		}
		if cleanup {
			return errors.New("Phone access process cleanup is incomplete; application must be preserved")
		}
	}
	return nil
}

// status --json is the public dashboard shape. The daemon's private snapshot
// uses tasks/groups and must not stand in for the real CLI readiness response.
func managedReadiness(raw []byte) error {
	value, err := decodeObject(raw)
	if err != nil {
		return errors.New("Managed daemon returned invalid readiness JSON")
	}
	queue := obj(value["queue"])
	jobs, jobsOK := value["jobs"].([]any)
	limit, limitOK := integer(queue["limit"])
	running, runningOK := integer(queue["running"])
	waiting, waitingOK := integer(queue["waiting"])
	_, timestampErr := time.Parse(time.RFC3339Nano, text(value["updatedAt"]))
	if queue == nil || !jobsOK || jobs == nil || obj(value["scheduler"]) == nil || !limitOK || limit < 1 || limit > MaxParallel || !runningOK || running < 0 || !waitingOK || waiting < 0 || timestampErr != nil || !slices.Contains([]string{"running", "paused"}, text(queue["status"])) {
		return errors.New("Managed daemon returned invalid readiness JSON")
	}
	return nil
}

// quiesceManaged runs with install.lock, scheduling.lock and remote control.lock
// held. It never publishes a bundle, rewrites wrappers or registers services.
func (install *installer) quiesceManaged(ctx context.Context, p *managedPlan) error {
	if p.live != nil {
		if p.live.adopted {
			return nil
		}
		if err := install.beginManagedLive(ctx, p.live); err != nil {
			return err
		}
	}
	for _, service := range []string{"resources", "remote", "queue", "login"} {
		if p.statuses[service] == "enabled" || install.unregisterServices && p.statuses[service] == "requiresApproval" {
			if label := map[string]string{"queue": "io.went.agent-heavy", "resources": "io.went.agent-resources", "remote": "io.went.agent-remote"}[service]; label != "" {
				output, loaded, err := install.inspectService(ctx, label)
				if err != nil {
					return err
				}
				if loaded {
					if err := install.managedLoaded(output, label, install.app); err != nil {
						return err
					}
				}
			}
			if service == "queue" && p.live != nil {
				if err := install.commitManagedLive(ctx, p.live); err != nil {
					return err
				}
			}
			if err := install.managedAction(ctx, install.app, "unregister", service); err != nil {
				return err
			}
		}
	}
	if err := install.stopManagedLegacy(ctx, p); err != nil {
		return err
	}
	if exists, err := updateExists(QueueDatabasePath(install.state)); err != nil {
		return err
	} else if exists {
		if p.live != nil {
			if err := install.validateManagedLive(ctx, p.live); err != nil {
				return err
			}
		} else if err := install.managedOfflineDrain(ctx); err != nil {
			return err
		}
	}
	if p.pueue {
		snapshot, err := install.legacySnapshot(ctx, p.node, false)
		if err != nil {
			return err
		}
		if err := installRequireDrained(obj(snapshot["tasks"])); err != nil {
			return err
		}
		data, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		if err := installAtomic(filepath.Join(install.state, "legacy-queue-import.json"), data, 0600); err != nil {
			return err
		}
	}
	if install.serviceSlot != "" {
		data, _ := json.Marshal(Object{"slot": install.serviceSlot})
		filename := filepath.Join(install.state, "legacy-slot.json")
		existing, err := appservices.ReadFile(install.home, filename, 16384, false)
		if err == nil {
			slot, err := installValidateSlot(existing)
			if err != nil || slot != install.serviceSlot {
				return errors.New("Retained queue lease setting changed; evidence preserved")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		} else if err := installAtomic(filename, data, 0600); err != nil {
			return err
		}
	}
	return install.managedStopped(ctx)
}

func (install *installer) executeManaged(ctx context.Context) error {
	if err := install.managedPublicationAccess(); err != nil {
		return err
	}
	if err := install.recoverManagedPublication(ctx); err != nil {
		return err
	}
	// All safety and drain checks precede private-directory/lock creation.
	source := install.app
	if !install.appBundle {
		source = filepath.Join(install.payload, "Agent Queue.app")
	}
	if err := install.managedVerify(ctx, source); err != nil {
		return err
	}
	if _, err := install.planManaged(ctx); err != nil {
		if !errors.Is(err, errInstallLegacyPhoneAccess) || !install.disablePhoneAccess {
			return err
		}
		// Explicit recovery remains ownership-checked and cannot precede drain.
		if err := install.validateRetirement(); err != nil {
			return err
		}
		node, err := installConfig(filepath.Join(install.config, "pueue.yml"))
		if err != nil {
			return err
		}
		if install.embeddedDefinition() {
			snapshot, err := install.managedSnapshot(ctx)
			if err != nil {
				return err
			}
			if obj(snapshot["tasks"]) == nil {
				return errors.New("Invalid queue snapshot during phone access recovery")
			}
			if err := installRequireDrained(obj(snapshot["tasks"])); err != nil {
				return err
			}
		} else {
			tasks, err := install.tasks(ctx, node, install.loaded(ctx, "io.went.agent-heavy"))
			if err != nil {
				return err
			}
			if err := installRequireDrained(tasks); err != nil {
				return err
			}
		}
		if err := install.requireLegacyCleanup(); err != nil {
			return err
		}
		if err := install.validatePhoneAccessRecovery(ctx); err != nil {
			return err
		}
		if _, err := install.run(ctx, filepath.Join(source, "Contents/MacOS/agent-remote"), "disable"); err != nil {
			return err
		}
		if _, err := install.planManaged(ctx); err != nil {
			return err
		}
	}
	for _, directory := range []string{install.root, install.state, install.config, filepath.Join(install.home, ".local/bin"), filepath.Join(install.state, "remote")} {
		if err := installDirectory(directory); err != nil {
			return err
		}
	}
	if !install.appBundle && filepath.Dir(install.app) != "/Applications" {
		parent, err := integrationParent(install.app, true)
		if err != nil {
			return err
		}
		parent.Close() // Existing publication-directory modes are never changed.
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
	remote, err := installLock(filepath.Join(install.state, "remote"), "control.lock")
	if err != nil {
		return err
	}
	defer remote.Close()
	if err := install.validatePayload(); err != nil {
		return err
	}
	p, err := install.planManaged(ctx)
	if err != nil {
		return err
	}
	if err := install.managedRetireSupersededReceipt(); err != nil {
		return err
	}
	if install.automaticSetup {
		if err := install.managedBackupLegacyDefinitions(p); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(install.identity, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := installAtomic(filepath.Join(install.root, "install-in-progress.json"), raw, 0600); err != nil {
		return err
	}
	if err := install.cleanupManagedRuntimes(ctx); err != nil {
		return err
	}
	if p.live != nil {
		defer install.cancelManagedLive(p.live)
	}
	if err := install.quiesceManaged(ctx, p); err != nil {
		return err
	}
	if !install.appBundle {
		if err := install.managedStopMenu(ctx, source); err != nil {
			return err
		}
	}
	expected := install.identity
	if err := install.publishManaged(ctx); err != nil {
		return err
	}
	published, err := managedCaptureBundle(install.app)
	if err != nil {
		return err
	}
	if !published.Build.Equal(expected) {
		return errors.New("Published application differs from the pending candidate; activation refused")
	}
	if err := install.managedValidateExpected(expected); err != nil {
		return err
	}
	if err := install.managedSupport(); err != nil {
		return err
	}
	if p.node != nil {
		current, err := installConfigCallback(p.node)
		if err != nil {
			return err
		}
		if !install.standardCleanupCallback(current) && current != installQuote(filepath.Join(install.home, ".local/bin/agent-heavy"))+" cleanup {{ id }}" {
			return errors.New("Custom cleanup callback cannot be silently changed during managed migration")
		}
		callback := installYAMLValue(installYAMLValue(p.node, "daemon"), "callback")
		if callback == nil {
			return errors.New("Missing migration callback")
		}
		callback.Value = installQuote(filepath.Join(install.home, ".local/bin/agent-heavy")) + " cleanup {{ id }}"
		data, err := yaml.Marshal(p.node)
		if err != nil {
			return err
		}
		if err := installAtomic(filepath.Join(install.config, "pueue.yml"), data, 0600); err != nil {
			return err
		}
	}
	metadata, err := json.MarshalIndent(appservices.Installation{SchemaVersion: 1, App: install.app, ServiceManagement: true}, "", "  ")
	if err != nil {
		return err
	}
	if err := installAtomic(filepath.Join(install.root, "installation.json"), append(metadata, '\n'), 0600); err != nil {
		return err
	}
	for _, service := range []string{"queue", "resources", "login"} {
		if err := managedRequireBundle(install.app, published); err != nil {
			return err
		}
		if err := install.managedAction(ctx, install.app, "register", service); err != nil {
			return err
		}
	}
	// Service Management can take well over 30 seconds to apply a fresh
	// registration, notably after Finder replaced a registered bundle.
	ready := false
	for attempt := 0; attempt < 120; attempt++ {
		output, err := install.run(ctx, filepath.Join(install.app, "Contents/MacOS/agent-heavy"), "status", "--json")
		if err == nil {
			if err := managedReadiness(output); err != nil {
				return err
			}
			ready = true
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if !ready {
		return errors.New("The managed queue daemon did not become ready; setup remains incomplete")
	}
	if p.python {
		if err := install.removeLegacy(); err != nil {
			return err
		}
	}
	if err := remote.Close(); err != nil {
		return err
	}
	// The shared remote controller owns migration and preserves credentials/CA.
	// Publish location before invoking it, but keep the authoritative pending marker.
	if p.remote || p.remoteCleanup {
		if _, err := install.run(ctx, filepath.Join(install.app, "Contents/MacOS/agent-remote"), "migrate"); err != nil {
			return fmt.Errorf("Managed phone access migration is incomplete: %w", err)
		}
	}
	// The remote controller re-reads the user's choice under control.lock.
	// Retain that lock through final readback/publication so a stale planning
	// snapshot cannot re-enable access after a concurrent explicit disable.
	remoteCommit, err := installLock(filepath.Join(install.state, "remote"), "control.lock")
	if err != nil {
		return err
	}
	defer remoteCommit.Close()
	remoteEnabled, err := install.remoteEnabled()
	if err != nil {
		return err
	}
	for _, service := range []string{"queue", "resources", "login"} {
		status, err := install.managedStatus(ctx, install.app, service)
		if err != nil {
			return err
		}
		if status != "enabled" {
			return fmt.Errorf("Managed service %s is %s; setup remains incomplete", service, status)
		}
	}
	for _, label := range []string{"io.went.agent-heavy", "io.went.agent-resources"} {
		output, loaded, err := install.inspectService(ctx, label)
		if err != nil {
			return err
		}
		if !loaded {
			return fmt.Errorf("Managed service %s is not loaded; setup remains incomplete", label)
		}
		if err := install.managedLoaded(output, label, install.app); err != nil {
			return err
		}
	}
	remoteStatus, err := install.managedStatus(ctx, install.app, "remote")
	if err != nil {
		return err
	}
	if !remoteEnabled && remoteStatus == "notFound" {
		// No registration is needed for disabled access. A lookup miss alone
		// cannot prove shutdown: independently verify no loaded job or cleanup.
		_, loaded, err := install.inspectService(ctx, "io.went.agent-remote")
		if err != nil {
			return err
		}
		cleanup, err := install.disabledRemoteCleanup(ctx)
		if err != nil {
			return err
		}
		if loaded || cleanup {
			return errors.New("Phone access lookup is unresolved and owned work remains; setup remains incomplete")
		}
	} else if remoteEnabled && remoteStatus != "enabled" || !remoteEnabled && remoteStatus != "notRegistered" {
		return errors.New("Phone access service does not match its retained enabled setting; setup remains incomplete")
	}
	if err := install.managedVerify(ctx, install.app); err != nil {
		return err
	}
	if err := install.managedValidateExpected(expected); err != nil {
		return err
	}
	if err := managedRequireBundle(install.app, published); err != nil {
		return err
	}
	// Retire only the two canonical owned executable files, after the remote
	// controller has adopted the bundle. Unknown private files remain untouched.
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		filename := filepath.Join(install.root, "bin", name)
		if _, err := appservices.ReadFile(install.home, filename, updateMaxArchive, false); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		parent, err := integrationParent(filename, false)
		if err != nil {
			return err
		}
		err = unix.Unlinkat(int(parent.Fd()), name, 0)
		parent.Close()
		if err != nil {
			return err
		}
	}
	if err := installAtomic(filepath.Join(install.root, "build-info.json"), raw, 0600); err != nil {
		return err
	}
	if p.live != nil {
		if err := install.retainManagedRuntime(ctx, p.live.Attempts); err != nil {
			return err
		}
	}
	if err := install.managedCleanupBackup(); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(install.root, "install-in-progress.json")); err != nil {
		return err
	}
	root, err := os.Open(install.root)
	if err != nil {
		return err
	}
	err = root.Sync()
	root.Close()
	if err != nil {
		return err
	}
	if p.live != nil {
		if err := install.finishManagedLive(p.live); err != nil {
			return err
		}
	}
	// mainApp registration only arranges subsequent logins. This one-shot,
	// explicit installer launch runs after durable completion, never as an agent.
	if !install.testing && !install.appBundle {
		if _, err := install.run(ctx, "/usr/bin/open", "-a", install.app); err != nil {
			return err
		}
	}
	return nil
}

// A crash can leave the last good app renamed but no candidate at the public
// path. Restore only that identity-checked owned backup, under installation and
// scheduling locks and after the same drained-queue gate used for replacement.
func (install *installer) recoverManagedPublication(ctx context.Context) error {
	if install.appBundle {
		return nil
	}
	if err := install.managedPublicationAccess(); err != nil {
		return err
	}
	if exists, err := updateExists(install.app); err != nil || exists {
		return err
	}
	previous := install.managedPreviousPath()
	if exists, err := updateExists(previous); err != nil || !exists {
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
	remote, err := installLock(filepath.Join(install.state, "remote"), "control.lock")
	if err != nil {
		return err
	}
	defer remote.Close()
	if err := install.managedBackupAccess(); err != nil {
		return err
	}
	receipt, raw, err := install.managedReadReceipt()
	if err != nil {
		return err
	}
	if receipt == nil || receipt.Backup == nil {
		return errors.New("Retained application has no exact owned publication receipt; evidence preserved")
	}
	if err := installValidatePayloadTree(previous, false); err != nil {
		return err
	}
	if _, err := install.run(ctx, "/usr/bin/codesign", "--verify", "--strict", "--verbose=2", previous); err != nil {
		return fmt.Errorf("Retained application signature verification failed: %w", err)
	}
	p := &managedPlan{loaded: map[string]bool{}}
	for _, label := range []string{"io.went.agent-heavy", "io.went.agent-resources", "io.went.agent-remote"} {
		output, loaded, err := install.inspectService(ctx, label)
		if err != nil {
			return err
		}
		if loaded {
			if err := install.managedLoaded(output, label, install.app); err != nil {
				return err
			}
		}
		p.loaded[label] = loaded
	}
	if err := install.managedDrain(ctx, p); err != nil {
		return err
	}
	parent, err := install.managedPublicationParent(true)
	if err != nil {
		return err
	}
	defer parent.Close()
	// Recheck after signature commands and drain inspection. Never overwrite a
	// target that appeared while recovery was checking the retained evidence.
	if err := install.managedBackupAccess(); err != nil {
		return err
	}
	if exists, err := updateExists(install.app); err != nil {
		return err
	} else if exists {
		return errors.New("Managed recovery target appeared during inspection; evidence preserved")
	}
	if err := managedRequireBundle(previous, *receipt.Backup); err != nil {
		return err
	}
	if err := unix.Renameat(int(parent.Fd()), filepath.Base(previous), int(parent.Fd()), filepath.Base(install.app)); err != nil {
		return err
	}
	if err := parent.Sync(); err != nil {
		return err
	}
	if err := managedRequireBundle(install.app, *receipt.Backup); err != nil {
		return err
	}
	return install.managedRemoveFile(install.managedReceiptPath(), raw)
}

func (install *installer) preflightManaged(ctx context.Context, report installPreflightReport) installPreflightReport {
	for _, directory := range []string{install.root, install.state, install.config, install.launch, filepath.Join(install.home, ".local/bin")} {
		if err := installInspectDirectory(directory); err != nil {
			report.add("private-path-unsafe", "Review private installation permissions", err.Error(), directory, "reveal")
			return report
		}
	}
	for _, entry := range []struct{ directory, name string }{{install.state, "install.lock"}, {install.state, "scheduling.lock"}, {filepath.Join(install.state, "remote"), "control.lock"}} {
		busy, err := installInspectLock(entry.directory, entry.name)
		if err != nil {
			report.add("private-path-unsafe", "Review coordination", err.Error(), entry.directory, "reveal")
		} else if busy {
			report.add("installer-busy", "Another queue operation is running", "Wait for that operation to finish.", "", "wait")
		}
	}
	source := install.app
	if !install.appBundle {
		source = filepath.Join(install.payload, "Agent Queue.app")
	}
	if err := install.managedVerify(ctx, source); err != nil {
		report.add("payload-invalid", "Review the signed application package", err.Error(), source, "reveal")
		return report
	}
	if missing, err := install.missingServiceDefinitions(ctx); err == nil && len(missing) > 0 {
		report.add("service-definitions-missing", "Restoring verified background service files", "Loaded legacy services have missing standard definitions. Setup will restore them without interrupting their jobs.", install.launch, installRestoreServices)
		return report
	}
	if _, err := install.planManaged(ctx); err != nil {
		var approval *installApprovalError
		var drain *installDrainPendingError
		switch {
		case errors.As(err, &approval):
			report.add("service-approval", "Allow Agent Queue background services", err.Error(), "", "wait")
		case errors.Is(err, errInstallPendingCleanup):
			if previous, loadErr := appservices.Load(install.home); loadErr == nil && previous != nil {
				snapshot, readErr := install.managedSnapshot(ctx)
				if readErr != nil || obj(snapshot["tasks"]) == nil {
					report.add("managed-setup-blocked", "Cannot inspect completed queue jobs", "The queue snapshot is unavailable or invalid; cleanup evidence was retained.", install.state, "reveal")
					break
				}
				report.add("pending-cleanup", "Finishing completed queue cleanup", err.Error(), install.state, "wait")
				install.preflightCleanupJobs(&report, true, obj(snapshot["tasks"]))
				break
			}
			return install.preflightLegacy(ctx, report)
		case errors.Is(err, errInstallCustomCallback), errors.Is(err, errInstallLegacyPhoneAccess):
			return install.preflightLegacy(ctx, report)
		case errors.As(err, &drain):
			install.preflightJobs(&report, drain.tasks, false)
			for i := range report.Issues {
				if report.Issues[i].ID == "legacy-jobs" {
					report.Issues[i].ID = "managed-jobs"
					report.Issues[i].Title = "Waiting for queue jobs"
					report.Issues[i].Detail = "Existing jobs continue while setup waits. Installation will resume automatically once the queue is drained. Paused or stashed jobs require an operator decision."
				}
			}
		default:
			report.add("managed-setup-blocked", "Review managed service setup", err.Error(), install.app, "reveal", "wait")
		}
	}
	return report
}
