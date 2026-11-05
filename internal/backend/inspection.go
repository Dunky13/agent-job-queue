package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Dunky13/agent-job-queue/internal/wire"
	"github.com/google/shlex"
)

var genericTitles = map[string]bool{"Shell batch": true, "Python batch": true, "Validation batch": true, "Active validation batch": true}

func DisplayText(value string, limit int) string {
	runes := []rune{}
	for _, r := range value {
		if !unicode.IsPrint(r) {
			r = ' '
		}
		runes = append(runes, r)
	}
	runes = []rune(strings.Join(strings.Fields(string(runes)), " "))
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}
func SafeSummary(argv []string) (string, string) {
	executable := ""
	if len(argv) > 0 {
		executable = DisplayText(filepath.Base(argv[0]), 80)
	}
	base := strings.ToLower(executable)
	if len(argv) > 1 && !strings.HasPrefix(argv[1], "-") {
		extension := strings.ToLower(filepath.Ext(argv[1]))
		if extension == ".sh" || extension == ".py" || extension == ".js" || extension == ".mjs" || extension == ".cjs" {
			return DisplayText(filepath.Base(argv[1]), 120), executable
		}
	}
	for _, shell := range []string{"sh", "bash", "zsh", "fish", "dash"} {
		if base == shell {
			return "Shell batch", executable
		}
	}
	if strings.HasPrefix(base, "python") {
		return "Python batch", executable
	}
	verbs := map[string]string{"typecheck": "Typecheck", "type-check": "Typecheck", "tsc": "Typecheck", "tsgo": "Typecheck", "test": "Tests", "tests": "Tests", "vitest": "Tests", "pytest": "Tests", "playwright": "Browser tests", "build": "Build", "lint": "Lint", "check": "Checks", "vet": "Go vet"}
	if base == "go" && len(argv) > 1 {
		title := map[string]string{"test": "Go tests", "build": "Go build", "vet": "Go vet"}[argv[1]]
		if title == "" {
			title = "Go batch"
		}
		return title, executable
	}
	for _, tool := range []string{"npm", "pnpm", "yarn", "bun", "node", "npx", "uv", "cargo", "make"} {
		if base == tool {
			for _, arg := range argv[1:min(4, len(argv))] {
				if title := verbs[arg]; title != "" {
					return title, executable
				}
			}
			return executable + " batch", executable
		}
	}
	if title := verbs[base]; title != "" {
		return title, executable
	}
	if executable == "" {
		return "Validation batch", ""
	}
	return executable, executable
}
func ProjectDescription(g *Gate, directory string) Object {
	if value, ok := g.ProjectCache[directory]; ok {
		return value
	}
	project := filepath.Base(directory)
	if project == "." || project == "/" {
		project = "Unknown project"
	}
	result := Object{"project": project, "branch": "", "directory": directory}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/git", "-C", directory, "rev-parse", "--path-format=absolute", "--git-common-dir", "--show-toplevel", "--abbrev-ref", "HEAD")
	cmd.Env = envList(g.ControlEnv)
	output, err := cmd.Output()
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		if len(lines) == 3 {
			if filepath.Base(lines[0]) == ".git" {
				result["project"] = filepath.Base(filepath.Dir(lines[0]))
			} else {
				result["project"] = filepath.Base(lines[1])
			}
			result["branch"] = DisplayText(lines[2], 120)
		}
	}
	result["project"] = DisplayText(text(result["project"]), 120)
	result["learningScope"] = profileDirectoryScope(directory)
	g.ProjectCache[directory] = result
	return result
}
func MakeDescription(g *Gate, argv []string, directory, label string) (Object, error) {
	title, executable := SafeSummary(argv)
	if label != "" {
		if strings.TrimSpace(label) == "" || len([]rune(label)) > 120 {
			return nil, errors.New("Label must be 1 to 120 printable characters")
		}
		for _, r := range label {
			if !unicode.IsPrint(r) {
				return nil, errors.New("Label must be 1 to 120 printable characters")
			}
		}
		title = strings.TrimSpace(label)
	}
	result := Object{}
	for k, v := range ProjectDescription(g, directory) {
		result[k] = v
	}
	result["title"] = title
	result["executable"] = executable
	if kind := profileCommandKind(argv, title, executable); kind != "" {
		result["learningKind"] = kind
	}
	result["learningTool"], result["learningOperation"] = profileClassify(argv, title, executable)
	return result, nil
}
func payloadLabel(payload Object) string {
	label := text(payload["label"])
	if boolean(payload["explicit_label"]) || !genericTitles[label] {
		return label
	}
	return ""
}
func CacheDescription(g *Gate, value string, description Object) error {
	if _, err := g.PayloadPath(value); err != nil {
		return err
	}
	if err := WritePrivate(filepath.Join(g.Metadata, value+".json"), description); err != nil {
		return err
	}
	g.DescriptionCache[value] = description
	return nil
}
func ReadDescription(g *Gate, value string) (Object, error) {
	if _, err := g.PayloadPath(value); err != nil {
		return nil, err
	}
	if cached := g.DescriptionCache[value]; cached != nil {
		return cached, nil
	}
	description, err := ReadPrivate(filepath.Join(g.Metadata, value+".json"))
	if err == nil {
		g.DescriptionCache[value] = description
	}
	return description, err
}
func CommandToken(task Object) string {
	if value := text(task["token"]); validToken(value) {
		return value
	}
	command, ok := task["command"].(string)
	if !ok {
		return ""
	}
	fields, err := shlex.Split(command)
	if err != nil || len(fields) < 2 || fields[len(fields)-2] != "execute" || !validToken(fields[len(fields)-1]) {
		return ""
	}
	return fields[len(fields)-1]
}
func DescribeTask(g *Gate, task Object, state string) (Object, error) {
	value := CommandToken(task)
	description := g.DescriptionCache[value]
	if value != "" && description == nil {
		var err error
		description, err = ReadDescription(g, value)
		if errors.Is(err, os.ErrNotExist) {
			path, _ := g.PayloadPath(value)
			payload, e := ReadJobPayload(path)
			if e != nil && !errors.Is(e, os.ErrNotExist) {
				return nil, e
			}
			if payload != nil {
				argv, e := stringsFrom(payload["argv"])
				if e != nil {
					return nil, e
				}
				description, err = MakeDescription(g, argv, text(payload["cwd"]), payloadLabel(payload))
			} else {
				err = nil
			}
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if description == nil {
		description = Object{}
		for k, v := range ProjectDescription(g, text(task["path"])) {
			description[k] = v
		}
		description["title"] = "Validation batch"
		if state == "running" {
			description["title"] = "Active validation batch"
		}
		description["executable"] = ""
	}
	result := Object{}
	for _, key := range []string{"project", "branch", "directory", "title", "executable"} {
		value, ok := description[key].(string)
		if !ok {
			return nil, errors.New("Invalid cached job description")
		}
		result[key] = value
	}
	explicitGeneric := false
	if value != "" && genericTitles[text(result["title"])] {
		path, _ := g.PayloadPath(value)
		payload, err := ReadJobPayload(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if payload != nil {
			explicitGeneric = boolean(payload["explicit_label"])
			argv, err := stringsFrom(payload["argv"])
			if err != nil {
				return nil, err
			}
			title, executable := SafeSummary(argv)
			if !explicitGeneric && !genericTitles[title] {
				result["title"] = title
				result["executable"] = executable
			}
		}
	}
	label, ok := task["label"].(string)
	if ok && strings.TrimSpace(label) != "" && (!genericTitles[label] || explicitGeneric || genericTitles[text(result["title"])]) {
		result["title"] = DisplayText(label, 120)
	}
	if value != "" {
		// The classification is known only at submission, so the cached file keeps
		// it; the returned description stays limited to the display fields.
		persisted := Object{}
		for k, v := range result {
			persisted[k] = v
		}
		for _, key := range []string{"learningTool", "learningOperation"} {
			if v, ok := description[key]; ok {
				persisted[key] = v
			}
		}
		if !jsonEqual(g.DescriptionCache[value], persisted) {
			if err := CacheDescription(g, value, persisted); err != nil {
				return nil, err
			}
		}
		g.DescriptionCache[value] = persisted
	}
	return result, nil
}
func LegacyOccupied(g *Gate) bool {
	if _, err := os.Lstat(g.Slot); errors.Is(err, os.ErrNotExist) {
		return false
	}
	unlock, err := LockPrivate(g, "")
	if err != nil {
		return true
	}
	defer unlock()
	if _, err := os.Lstat(g.Slot); errors.Is(err, os.ErrNotExist) {
		return false
	}
	manifest, err := SharedManifest(g)
	return err != nil || manifest == nil
}
func StatusData(g *Gate) (wire.Snapshot, error) {
	if err := g.Prepare(); err != nil {
		return wire.Snapshot{}, err
	}
	snapshot, err := g.RecentSnapshot(recentDoneJobs)
	if err != nil {
		return wire.Snapshot{}, err
	}
	scheduling, err := SchedulingView(g)
	if err != nil {
		return wire.Snapshot{}, err
	}
	jobs := []wire.Job{}
	doneEntries := []Object{}
	entries := []Object{}
	for key, raw := range obj(snapshot["tasks"]) {
		id, err := strconv.Atoi(key)
		if err != nil || id < 0 {
			return wire.Snapshot{}, errors.New("Invalid task ID")
		}
		task := obj(raw)
		if task == nil {
			return wire.Snapshot{}, errors.New("Invalid task")
		}
		name, details, err := TaskStatus(task)
		if err != nil {
			return wire.Snapshot{}, err
		}
		entry := Object{"id": id, "task": task, "end": details["end"]}
		if name == "Done" {
			doneEntries = append(doneEntries, entry)
		} else {
			entries = append(entries, entry)
		}
	}
	sort.Slice(doneEntries, func(i, j int) bool {
		a, b := text(doneEntries[i]["end"]), text(doneEntries[j]["end"])
		if a != b {
			return a > b
		}
		ai, _ := integer(doneEntries[i]["id"])
		bi, _ := integer(doneEntries[j]["id"])
		return ai > bi
	})
	entries = append(entries, doneEntries[:min(recentDoneJobs, len(doneEntries))]...)
	lanes := obj(snapshot["lanes"])
	for _, entry := range entries {
		id, _ := integer(entry["id"])
		task := obj(entry["task"])
		name, details, err := TaskStatus(task)
		if err != nil {
			return wire.Snapshot{}, err
		}
		if taskIsLease(task) {
			job, err := leaseStatusJob(id, task, details, scheduling)
			if err != nil {
				return wire.Snapshot{}, err
			}
			jobs = append(jobs, job)
			continue
		}
		state := strings.ToLower(name)
		receipt, err := g.Receipt(id)
		if err != nil {
			return wire.Snapshot{}, err
		}
		value := CommandToken(task)
		resources := obj(obj(scheduling["jobs"])[value])
		ownedWaiting := name == "Stashed" && value != ""
		if ownedWaiting {
			state = "queued"
		}
		phase := name
		if name == "Running" && receipt != nil && receipt["pgid"] == nil {
			phase = "Waiting for shared validation slot"
		}
		if (ownedWaiting || name == "Queued") && scheduling["mode"] == "adaptive" {
			phase = text(obj(resources["decision"])["reason"])
			if phase == "" {
				phase = "Waiting for resource admission"
			}
		}
		if name == "Done" && receipt != nil {
			phase = "Finishing cleanup"
		}
		priority := 0
		if raw, present := task["priority"]; present {
			valid := false
			priority, valid = integer(raw)
			if !valid {
				return wire.Snapshot{}, errors.New("Queue returned an invalid priority")
			}
		}
		description, err := DescribeTask(g, task, state)
		if err != nil {
			return wire.Snapshot{}, err
		}
		job := wire.Job{ID: id, State: state, Priority: priority, SubmittedAt: nullableText(task["created_at"]), StartedAt: nullableText(details["start"]), FinishedAt: nullableText(details["end"]), LogPath: g.LogPathFromTask(id, task), CanReorder: name == "Queued" || ownedWaiting, CanStart: name == "Queued" || ownedWaiting, CanCancel: name == "Queued" || name == "Stashed" || name == "Running" || name == "Paused", Phase: phase, Project: text(description["project"]), Branch: text(description["branch"]), Directory: text(description["directory"]), Title: text(description["title"]), Executable: text(description["executable"]), Lane: taskLane(task), Kind: "command"}
		if name == "Done" {
			code, err := ResultCode(details["result"])
			if err != nil {
				return wire.Snapshot{}, err
			}
			job.ExitCode = &code
		}
		if resources != nil {
			value, err := decodeWire[wire.Resources](resources)
			if err != nil {
				return wire.Snapshot{}, err
			}
			job.Resources = &value
		}
		jobs = append(jobs, job)
	}
	active, waiting, done := []wire.Job{}, []wire.Job{}, []wire.Job{}
	for _, job := range jobs {
		switch job.State {
		case "running", "paused", "held":
			active = append(active, job)
		case "done", "released", "expired", "dropped":
			done = append(done, job)
		default:
			waiting = append(waiting, job)
		}
	}
	lanePriority := func(job wire.Job) int {
		priority, _ := integer(obj(lanes[job.Lane])["priority"])
		return priority
	}
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })
	sort.Slice(waiting, func(i, j int) bool {
		if a, b := lanePriority(waiting[i]), lanePriority(waiting[j]); a != b {
			return a > b
		}
		if a, b := waiting[i].Priority, waiting[j].Priority; a != b {
			return a > b
		}
		return waiting[i].ID < waiting[j].ID
	})
	position := 0
	lanePositions := map[string]int{}
	for index := range waiting {
		job := &waiting[index]
		if job.State == "queued" {
			position++
			job.QueuePosition = new(position)
			lanePositions[job.Lane]++
			if job.Kind == "lease" {
				job.Lease.Position = new(lanePositions[job.Lane])
			}
		}
	}
	sort.Slice(done, func(i, j int) bool {
		a, b := pointerText(done[i].FinishedAt), pointerText(done[j].FinishedAt)
		if a != b {
			return a > b
		}
		return done[i].ID > done[j].ID
	})
	all := append(append(active, waiting...), done...)
	group := defaultGroup(snapshot)
	limit, err := decodeWire[*int](group["parallel_tasks"])
	if err != nil {
		return wire.Snapshot{}, err
	}
	scheduler, err := decodeWire[wire.Scheduler](scheduling)
	if err != nil {
		return wire.Snapshot{}, err
	}
	status := strings.ToLower(text(group["status"]))
	if status == "" {
		status = "unknown"
	}
	statusLaneList, err := statusLanes(snapshot, limit, len(active), len(waiting))
	if err != nil {
		return wire.Snapshot{}, err
	}
	plugins, err := statusPlugins(snapshot)
	if err != nil {
		return wire.Snapshot{}, err
	}
	queue := wire.Queue{Status: status, Limit: limit, Running: len(active), Waiting: len(waiting), LegacyOccupied: LegacyOccupied(g)}
	// Set only while intake is closed, or when the drain file cannot be trusted.
	if until, draining, err := drainUntil(g); err != nil {
		queue.DrainError = err.Error()
	} else if draining {
		queue.DrainUntil = until.UTC().Format(time.RFC3339)
	}
	return wire.Snapshot{UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: queue, Jobs: all, Scheduler: &scheduler, Lanes: statusLaneList, Plugins: plugins}, nil
}

// decodeWire converts a value the daemon or resource controller owns into its
// wire type. Keys the wire type does not declare are not published.
func decodeWire[T any](value any) (T, error) {
	var result T
	data, err := json.Marshal(value)
	if err != nil {
		return result, err
	}
	if err = json.Unmarshal(data, &result); err != nil {
		return result, fmt.Errorf("Queue returned invalid status data: %w", err)
	}
	return result, nil
}

func nullableText(value any) *string {
	if result, ok := value.(string); ok {
		return &result
	}
	return nil
}

func pointerText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// statusLanes lists every lane in priority order. A daemon that predates lanes
// publishes none; its queue is reported as the default lane alone.
func statusLanes(snapshot Object, limit *int, running, waiting int) ([]wire.Lane, error) {
	published := obj(snapshot["lanes"])
	if published == nil {
		return []wire.Lane{{Name: defaultLaneName, Label: "Default", Concurrency: limit, SharesLimit: true, Running: running, Waiting: waiting}}, nil
	}
	lanes := []wire.Lane{}
	for _, lane := range sortedLanes(published) {
		value, err := decodeWire[wire.Lane](lane)
		if err != nil {
			return nil, err
		}
		lanes = append(lanes, value)
	}
	return lanes, nil
}

func statusPlugins(snapshot Object) ([]wire.Plugin, error) {
	plugins := []wire.Plugin{}
	for _, raw := range list(snapshot["plugins"]) {
		if plugin := obj(raw); plugin != nil {
			value, err := decodeWire[wire.Plugin](plugin)
			if err != nil {
				return nil, err
			}
			plugins = append(plugins, value)
		}
	}
	return plugins, nil
}

// leaseStatusJob keeps every field existing status consumers require, with
// empty project and log values: a lease has neither a command nor output.
func leaseStatusJob(id int, task, details, scheduling Object) (wire.Job, error) {
	fields := leaseFields(id, task, nil)
	state := text(fields["state"])
	phase := map[string]string{"queued": "Waiting for lane capacity", "held": "Held", "released": "Released", "expired": "Expired", "dropped": "Dropped"}[state]
	if state == "queued" && scheduling["mode"] == "adaptive" {
		if reason := text(obj(obj(obj(scheduling["leases"])[strconv.Itoa(id)])["decision"])["reason"]); reason != "" {
			phase = reason
		}
	}
	if reason := text(fields["reason"]); reason != "" && state != "queued" && state != "held" {
		phase += " (" + strings.ReplaceAll(reason, "-", " ") + ")"
	}
	title := text(task["label"])
	if strings.TrimSpace(title) == "" {
		title = "Lease"
	}
	priority, _ := integer(task["priority"])
	lease, err := decodeWire[wire.Lease](fields)
	if err != nil {
		return wire.Job{}, err
	}
	return wire.Job{ID: id, State: state, Title: DisplayText(title, 120), Priority: priority, SubmittedAt: nullableText(task["created_at"]), StartedAt: nullableText(details["start"]), FinishedAt: nullableText(details["end"]), CanReorder: state == "queued", CanStart: state == "queued", CanCancel: state == "queued" || state == "held", Phase: phase, Kind: "lease", Lane: text(fields["lane"]), Lease: &lease}, nil
}
func ValidateLimit(value int) error {
	if value < 1 || value > MaxParallel {
		return fmt.Errorf("Parallel limit must be from 1 to %d; zero means unlimited and is forbidden", MaxParallel)
	}
	return nil
}
func ValidatePriority(value int) error {
	if value < -(1<<31) || value > (1<<31)-1 {
		return errors.New("Priority must be a signed 32-bit integer")
	}
	return nil
}
func PersistedLimit(g *Gate) (int, error) {
	s, e := openQueueStore(g.State, false)
	if e != nil {
		return 0, e
	}
	defer s.close()
	settings, e := s.settings()
	if e != nil {
		return 0, e
	}
	n, ok := integer(settings["parallel_tasks"])
	if !ok {
		return 0, errors.New("Invalid persisted queue limit")
	}
	return n, nil
}
func SetLimit(g *Gate, value int) error {
	if e := ValidateLimit(value); e != nil {
		return e
	}
	if e := g.Prepare(); e != nil {
		return e
	}
	result, e := g.QueueControl("limit", Object{"limit": value})
	if e != nil {
		return e
	}
	n, ok := integer(result["parallel_tasks"])
	if !ok || n != value {
		return errors.New("Queue limit was not acknowledged")
	}
	return nil
}
func StartJob(g *Gate, id int) error {
	if id < 0 {
		return errors.New("Task ID must be nonnegative")
	}
	if err := g.Prepare(); err != nil {
		return err
	}
	previous := g.ControlDeadline
	g.ControlDeadline = time.Now().Add(20 * time.Second)
	defer func() { g.ControlDeadline = previous }()
	unlock, err := LockPrivate(g, "scheduling.lock")
	if err != nil {
		return err
	}
	defer unlock()
	snapshot, err := g.ActiveSnapshot()
	if err != nil {
		return err
	}
	task := snapshotTaskByID(snapshot, id)
	name, _, err := TaskStatus(task)
	if err != nil {
		return err
	}
	value := CommandToken(task)
	lease := taskIsLease(task)
	if (name != "Queued" && name != "Stashed") || value == "" && !lease {
		return fmt.Errorf("Only a queued agent-heavy job can be started; inspect job %d", id)
	}
	// Start now overrides the global limit and resource admission, never a
	// lane's own concurrency. The daemon recounts; this check explains early.
	if lane := snapshotLane(snapshot, taskLane(task)); taskLane(task) != defaultLaneName && lane != nil {
		concurrency, _ := integer(lane["concurrency"])
		running, _ := integer(lane["running"])
		held, _ := integer(lane["held"])
		if running+held >= concurrency {
			return fmt.Errorf("%w (%s: %d of %d in use); Start now cannot exceed a lane's concurrency", errQueueLaneFull, taskLane(task), running+held, concurrency)
		}
	}
	if !lease {
		if err = ManualGrant(g, id, value); err != nil {
			return err
		}
	}
	if _, err = g.QueueControl("start", Object{"id": id}); err != nil {
		if errors.Is(err, errQueueLaneFull) && !lease {
			_ = revokeManualGrant(g, id, value)
		}
		return err
	}
	task, err = g.Task(id)
	if err != nil {
		return err
	}
	name, _, err = TaskStatus(task)
	if err != nil {
		return err
	}
	if name != "Running" && name != "Done" {
		return fmt.Errorf("Job %d start could not be verified", id)
	}
	return nil
}
func Reorder(g *Gate, id int, action string, priority int) (int, error) {
	if id < 0 {
		return 0, errors.New("Task ID must be nonnegative")
	}
	if action != "next" && action != "last" && action != "priority" {
		return 0, errors.New("Invalid reorder action")
	}
	if e := ValidatePriority(priority); e != nil {
		return 0, e
	}
	result, e := g.QueueControl(action, Object{"id": id, "priority": priority})
	if e != nil {
		return 0, e
	}
	n, ok := integer(result["priority"])
	if !ok {
		return 0, errors.New("Invalid acknowledged priority")
	}
	return n, nil
}
func (g *Gate) LogPathFromTask(id int, task Object) string {
	if path := text(task["logPath"]); path != "" {
		return path
	}
	return filepath.Join(g.State, "logs", strconv.Itoa(id)+".log")
}
func (g *Gate) LogPath(id int) string {
	task, e := g.Task(id)
	if e == nil && task != nil {
		return g.LogPathFromTask(id, task)
	}
	return filepath.Join(g.State, "logs", strconv.Itoa(id)+".log")
}
