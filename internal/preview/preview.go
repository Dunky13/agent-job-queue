// Package preview serves an ephemeral dashboard with simulated queue data. It
// never loads installed queue settings or executes the queue controller.
package preview

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Dunky13/agent-job-queue/internal/remote"
)

type fakeQueue struct {
	mu              sync.Mutex
	root            string
	events          [][]string
	offline         bool
	limit           int
	scheduler       map[string]any
	profileRevision int
	profiles        map[string]map[string]any
	jobs            []map[string]any
}

func clone[T any](value T) T {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var copy T
	if err := json.Unmarshal(data, &copy); err != nil {
		panic(err)
	}
	return copy
}
func (q *fakeQueue) reset() error {
	q.events = make([][]string, 0)
	q.offline = false
	q.limit = 1
	q.scheduler = nil
	q.profileRevision = 4
	q.profiles = learningProfiles()
	q.jobs = nil
	for index, title := range []string{"Build app", "Run browser tests", "Typecheck client"} {
		id, state := index+101, "queued"
		var position any = index
		var started any
		if index == 0 {
			state = "running"
			position = nil
			started = "2026-01-01T10:01:00+00:00"
		}
		log := filepath.Join(q.root, strconv.Itoa(id)+".log")
		if err := os.WriteFile(log, []byte("Starting "+title+"\nPassed fixture step\n"), 0600); err != nil {
			return err
		}
		q.jobs = append(q.jobs, map[string]any{"id": id, "state": state, "title": title, "project": "example-project", "branch": "feature/queue", "directory": "/example/project", "executable": "fixture-command", "priority": 0, "queuePosition": position, "submittedAt": "2026-01-01T10:00:00+00:00", "startedAt": started, "finishedAt": nil, "exitCode": nil, "logPath": log, "canReorder": state == "queued", "canStart": state == "queued", "canCancel": true, "phase": strings.ToUpper(state[:1]) + state[1:]})
	}
	return nil
}

func learningProfiles() map[string]map[string]any {
	detail := func(id, label string, families []string) map[string]any {
		members := make([]map[string]any, 0, len(families))
		for index, family := range families {
			measurements := map[string]any{"variantCount": 0, "successCount": 0, "incompleteCount": 0, "peakMemoryBytes": 0, "peakCpuCores": 0, "lastRunAt": nil}
			if index == 0 {
				measurements = map[string]any{"variantCount": 1, "successCount": 5, "incompleteCount": 1, "peakMemoryBytes": 1610612736, "peakCpuCores": 1.5, "lastRunAt": 1792404000}
			}
			members = append(members, map[string]any{"family": family, "label": fmt.Sprintf("%s task %d", label, index+1), "project": "example-project", "executable": "go", "measurements": measurements})
		}
		return map[string]any{
			"revision": 4,
			"profile":  map[string]any{"id": id, "label": label, "memberCount": len(members), "variantCount": 1, "successCount": 5, "incompleteCount": 1, "peakMemoryBytes": 1610612736, "peakCpuCores": 1.5, "lastRunAt": 1792404000, "memoryFloorBytes": nil, "cpuFloorCores": nil},
			"members":  members, "controllerAppliedRevision": 3, "controllerStatus": "Waiting for next fresh pass",
			"variants":          []map[string]any{{"id": strings.Repeat("f", 64), "family": families[0], "successCount": 5, "incompleteCount": 1, "lastRunAt": 1792404000, "peakMemoryBytes": 1610612736, "peakCpuCores": 1.5}},
			"runs":              []map[string]any{{"id": strings.Repeat("9", 64), "family": families[0], "variant": strings.Repeat("f", 64), "machine": strings.Repeat("2", 64), "sampledAt": 1792404000, "memoryBytes": 1610612736, "cpuCores": 1.5, "cpuSeconds": 20, "durationSeconds": 15, "success": false}},
			"prediction":        map[string]any{"memoryBytes": 2147483648, "cpuCores": 2, "durationSeconds": 60, "samples": 0, "confidence": "unknown", "source": "family"},
			"predictionMachine": strings.Repeat("2", 64), "truncated": map[string]any{"members": false, "variants": false, "runs": false},
		}
	}
	return map[string]map[string]any{strings.Repeat("a", 64): detail(strings.Repeat("a", 64), "Browser tests", []string{strings.Repeat("c", 64), strings.Repeat("d", 64)}), strings.Repeat("b", 64): detail(strings.Repeat("b", 64), "Client build", []string{strings.Repeat("e", 64)})}
}

func object(value any) (map[string]any, bool) {
	result, ok := value.(map[string]any)
	return result, ok
}
func members(value any) ([]map[string]any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var result []map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (q *fakeQueue) profilesCall(arguments []string, input []byte) (map[string]any, error) {
	action := arguments[1]
	if action == "list" {
		offset, _ := strconv.Atoi(arguments[3])
		limit, _ := strconv.Atoi(arguments[5])
		query := strings.ToLower(arguments[7])
		ids := make([]string, 0, len(q.profiles))
		for id := range q.profiles {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		profiles := make([]map[string]any, 0)
		for _, id := range ids {
			profile, ok := object(q.profiles[id]["profile"])
			if !ok {
				return nil, errors.New("Invalid fixture profile")
			}
			label, _ := profile["label"].(string)
			if strings.Contains(strings.ToLower(label), query) {
				profiles = append(profiles, clone(profile))
			}
		}
		total := len(profiles)
		start := min(offset, total)
		end := min(start+limit, total)
		return map[string]any{"revision": q.profileRevision, "profiles": profiles[start:end], "total": total, "offset": offset, "limit": limit, "controllerAppliedRevision": 3, "controllerStatus": "Waiting for next fresh pass"}, nil
	}
	id := arguments[2]
	detail, ok := q.profiles[id]
	if !ok {
		return nil, errors.New("Workload profile no longer exists; refresh learning")
	}
	if action == "show" {
		result, err := inspectProfile(detail, arguments[3:])
		if err != nil {
			return nil, err
		}
		result["revision"] = q.profileRevision
		return result, nil
	}
	var value map[string]any
	if err := json.Unmarshal(input, &value); err != nil {
		return nil, err
	}
	if value["revision"] != float64(q.profileRevision) {
		return nil, errors.New("Learning changed; refresh before saving corrections")
	}
	if action == "combine" {
		return q.combineProfiles(id, value, arguments)
	}
	q.events = append(q.events, append([]string(nil), arguments...))
	profile, ok := object(detail["profile"])
	if !ok {
		return nil, errors.New("Invalid fixture profile")
	}
	switch action {
	case "edit":
		for _, key := range []string{"label", "memoryFloorBytes", "cpuFloorCores"} {
			profile[key] = value[key]
		}
	case "reset":
		detail["runs"] = []any{}
		detail["variants"] = []any{}
		for _, key := range []string{"successCount", "incompleteCount", "variantCount", "peakMemoryBytes", "peakCpuCores"} {
			profile[key] = 0
		}
		profile["lastRunAt"] = nil
		all, err := members(detail["members"])
		if err != nil {
			return nil, err
		}
		for _, member := range all {
			member["measurements"] = map[string]any{"variantCount": 0, "successCount": 0, "incompleteCount": 0, "peakMemoryBytes": 0, "peakCpuCores": 0, "lastRunAt": nil}
		}
		detail["members"] = all
	case "merge":
		targetID, _ := value["targetId"].(string)
		target, exists := q.profiles[targetID]
		if !exists {
			return nil, errors.New("Fixture merge target no longer exists")
		}
		merged, err := members(target["members"])
		if err != nil {
			return nil, err
		}
		added, err := members(detail["members"])
		if err != nil {
			return nil, err
		}
		merged = append(merged, added...)
		target["members"] = merged
		for _, key := range []string{"variants", "runs"} {
			existing, err := members(target[key])
			if err != nil {
				return nil, err
			}
			added, err := members(detail[key])
			if err != nil {
				return nil, err
			}
			target[key] = append(existing, added...)
		}
		if err := retainProfileMembers(target); err != nil {
			return nil, err
		}
		targetProfile, ok := object(target["profile"])
		if !ok {
			return nil, errors.New("Invalid fixture profile")
		}
		targetProfile["memberCount"] = len(merged)
		delete(q.profiles, id)
		detail = target
	case "split":
		all, err := members(detail["members"])
		if err != nil {
			return nil, err
		}
		index := slices.IndexFunc(all, func(member map[string]any) bool { return member["family"] == value["family"] })
		if index < 0 {
			return nil, errors.New("Fixture member no longer exists")
		}
		member := all[index]
		separate := clone(detail)
		all = slices.Delete(all, index, index+1)
		detail["members"] = all
		profile["memberCount"] = len(all)
		separateProfile, ok := object(separate["profile"])
		if !ok {
			return nil, errors.New("Invalid fixture profile")
		}
		separateProfile["id"] = strings.Repeat("3", 64)
		separateProfile["label"] = member["label"]
		separateProfile["memberCount"] = 1
		separate["members"] = []map[string]any{member}
		for _, entry := range []map[string]any{detail, separate} {
			if err := retainProfileMembers(entry); err != nil {
				return nil, err
			}
		}
		q.profiles[strings.Repeat("3", 64)] = separate
		detail = separate
	default:
		return nil, errors.New("Unknown fixture profile action")
	}
	q.profileRevision++
	result := clone(detail)
	result["revision"] = q.profileRevision
	return result, nil
}

func inspectProfile(detail map[string]any, arguments []string) (map[string]any, error) {
	result := clone(detail)
	family, query, offset, limit := "", "", 0, 256
	for i := 0; i < len(arguments); i++ {
		key := arguments[i]
		if key == "--json" {
			continue
		}
		i++
		if i >= len(arguments) {
			return nil, errors.New("Missing fixture inspection value")
		}
		switch key {
		case "--family":
			family = arguments[i]
		case "--members-query":
			query = arguments[i]
		case "--members-offset":
			offset, _ = strconv.Atoi(arguments[i])
		case "--members-limit":
			limit, _ = strconv.Atoi(arguments[i])
		default:
			return nil, errors.New("Unknown fixture inspection option")
		}
	}
	if offset < 0 || limit < 1 || limit > 256 {
		return nil, errors.New("Invalid fixture member page")
	}
	all, err := members(result["members"])
	if err != nil {
		return nil, err
	}
	matching := []map[string]any{}
	for _, member := range all {
		if family != "" && member["family"] != family {
			continue
		}
		if family != "" || query == "" || strings.Contains(strings.ToLower(fmt.Sprint(member["label"], " ", member["project"], " ", member["executable"], " ", member["family"])), strings.ToLower(query)) {
			matching = append(matching, member)
		}
	}
	if family != "" {
		if len(matching) != 1 {
			return nil, errors.New("That workload does not belong to this group")
		}
		result["inspectedFamily"] = family
	}
	start := min(offset, len(matching))
	end := start + min(limit, len(matching)-start)
	result["members"] = matching[start:end]
	result["memberPage"] = map[string]any{"total": len(matching), "offset": offset, "limit": limit, "query": query}
	truncated := map[string]any{"members": len(matching) > end-start, "variants": false, "runs": false}
	for key, maximum := range map[string]int{"variants": 100, "runs": 30} {
		rows, err := members(result[key])
		if err != nil {
			return nil, err
		}
		if family != "" {
			rows = slices.DeleteFunc(rows, func(row map[string]any) bool { return row["family"] != family })
		}
		truncated[key] = len(rows) > maximum
		result[key] = rows[:min(maximum, len(rows))]
	}
	result["truncated"] = truncated
	return result, nil
}

func retainProfileMembers(detail map[string]any) error {
	all, err := members(detail["members"])
	if err != nil {
		return err
	}
	families := map[string]bool{}
	for _, member := range all {
		family, _ := member["family"].(string)
		families[family] = true
	}
	for _, key := range []string{"variants", "runs"} {
		rows, err := members(detail[key])
		if err != nil {
			return err
		}
		detail[key] = slices.DeleteFunc(rows, func(row map[string]any) bool { family, _ := row["family"].(string); return !families[family] })
	}
	profile, ok := object(detail["profile"])
	if !ok {
		return errors.New("Invalid fixture profile")
	}
	for _, key := range []string{"variantCount", "successCount", "incompleteCount", "peakMemoryBytes", "peakCpuCores"} {
		profile[key] = float64(0)
	}
	profile["lastRunAt"] = nil
	for _, member := range all {
		measurements, ok := object(member["measurements"])
		if !ok {
			return errors.New("Fixture member has no measurements")
		}
		for _, key := range []string{"variantCount", "successCount", "incompleteCount"} {
			value, _ := measurements[key].(float64)
			current, _ := profile[key].(float64)
			profile[key] = current + value
		}
		for _, key := range []string{"peakMemoryBytes", "peakCpuCores", "lastRunAt"} {
			value, ok := measurements[key].(float64)
			current, _ := profile[key].(float64)
			if ok && value > current {
				profile[key] = value
			}
		}
	}
	return nil
}

func (q *fakeQueue) run(_ context.Context, arguments []string, input []byte) (map[string]any, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.offline {
		return nil, errors.New("Fixture queue is offline")
	}
	if arguments[0] == "profiles" {
		return q.profilesCall(arguments, input)
	}
	if slices.Equal(arguments, []string{"status", "--json"}) {
		running, waiting := 0, 0
		for _, job := range q.jobs {
			if job["state"] == "running" {
				running++
			}
			if job["state"] == "queued" {
				waiting++
			}
		}
		value := map[string]any{"updatedAt": time.Now().UTC().Format(time.RFC3339Nano), "queue": map[string]any{"status": "running", "limit": q.limit, "running": running, "waiting": waiting, "legacyOccupied": false}, "jobs": clone(q.jobs)}
		if q.scheduler != nil {
			value["scheduler"] = clone(q.scheduler)
		}
		return value, nil
	}
	q.events = append(q.events, append([]string(nil), arguments...))
	switch arguments[0] {
	case "limit":
		q.limit, _ = strconv.Atoi(arguments[1])
	case "scheduling":
		if q.scheduler == nil {
			return nil, errors.New("Resource sampler unavailable")
		}
		if available, _ := q.scheduler["available"].(bool); arguments[1] == "adaptive" && !available {
			return nil, errors.New("Resource sampler unavailable")
		}
		q.scheduler["mode"] = arguments[1]
	default:
		id, err := strconv.Atoi(arguments[1])
		if err != nil {
			return nil, err
		}
		index := slices.IndexFunc(q.jobs, func(job map[string]any) bool { return job["id"] == id })
		if index < 0 {
			return nil, errors.New("Fixture job no longer exists")
		}
		job := q.jobs[index]
		switch arguments[0] {
		case "start":
			job["state"] = "running"
			job["phase"] = "Running"
			job["queuePosition"] = nil
			job["canStart"] = false
			job["canReorder"] = false
			job["startedAt"] = "2026-01-01T10:02:00+00:00"
		case "cancel":
			job["state"] = "cancelled"
			job["phase"] = "Cancelled"
			job["queuePosition"] = nil
			job["canStart"] = false
			job["canCancel"] = false
			job["canReorder"] = false
		case "next", "last":
			waiting := make([]map[string]any, 0)
			if arguments[0] == "next" {
				waiting = append(waiting, job)
			}
			for other, candidate := range q.jobs {
				if other != index && candidate["state"] == "queued" {
					waiting = append(waiting, candidate)
				}
			}
			if arguments[0] == "last" {
				waiting = append(waiting, job)
			}
			for position, candidate := range waiting {
				candidate["queuePosition"] = position + 1
			}
		default:
			return nil, errors.New("Unknown fixture queue action")
		}
	}
	return map[string]any{"ok": true}, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (q *fakeQueue) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if r.Method == "GET" && r.URL.Path == "/__fixture/events" {
		writeJSON(w, 200, map[string]any{"events": q.events})
		return
	}
	if r.Method == "POST" && r.URL.Path == "/__fixture/reset" {
		if err := q.reset(); err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	if r.Method != "POST" || r.URL.Path != "/__fixture/state" {
		writeJSON(w, 404, map[string]any{"error": "Not Found"})
		return
	}
	var values map[string]json.RawMessage
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&values); err != nil || values == nil {
		writeJSON(w, 400, map[string]any{"error": "Invalid fixture state"})
		return
	}
	fields := []string{"profileFloors", "profileRevision", "profileCount", "profileMemberCount", "profiles", "offline", "title", "scheduler", "resources"}
	for key := range values {
		if !slices.Contains(fields, key) {
			writeJSON(w, 400, map[string]any{"error": "Unknown fixture state field"})
			return
		}
	}
	for _, key := range fields {
		value, exists := values[key]
		if !exists {
			continue
		}
		var err error
		switch key {
		case "profileFloors":
			var floors map[string]any
			err = json.Unmarshal(value, &floors)
			if err == nil {
				if detail, ok := q.profiles[strings.Repeat("a", 64)]; ok {
					if profile, ok := object(detail["profile"]); ok {
						for field, floor := range floors {
							profile[field] = floor
						}
					}
				}
			}
		case "profileRevision":
			err = json.Unmarshal(value, &q.profileRevision)
		case "profileCount":
			var count int
			err = json.Unmarshal(value, &count)
			if err == nil && (count < 0 || count > 10000) {
				err = errors.New("Invalid fixture profile count")
			}
			if err == nil {
				template := learningProfiles()[strings.Repeat("a", 64)]
				q.profiles = make(map[string]map[string]any, count)
				for index := range count {
					id := fmt.Sprintf("%064x", index)
					detail := clone(template)
					profile, _ := object(detail["profile"])
					profile["id"] = id
					profile["label"] = fmt.Sprintf("Workload %d", index)
					all, memberErr := members(detail["members"])
					if memberErr != nil {
						err = memberErr
						break
					}
					for memberIndex, member := range all {
						oldFamily := member["family"]
						family := fmt.Sprintf("%064x", 1000000+index*2+memberIndex)
						member["family"] = family
						member["label"] = fmt.Sprintf("Workload %d task %d", index, memberIndex+1)
						for _, key := range []string{"variants", "runs"} {
							rows, rowErr := members(detail[key])
							if rowErr != nil {
								err = rowErr
								break
							}
							for _, row := range rows {
								if row["family"] == oldFamily {
									row["family"] = family
								}
							}
							detail[key] = rows
						}
					}
					detail["members"] = all
					q.profiles[id] = detail
				}
			}
		case "profileMemberCount":
			var count int
			err = json.Unmarshal(value, &count)
			if err == nil && (count < 1 || count > 10000) {
				err = errors.New("Invalid fixture member count")
			}
			if err == nil {
				detail, exists := q.profiles[strings.Repeat("a", 64)]
				if !exists {
					err = errors.New("Missing fixture profile")
					break
				}
				all := make([]map[string]any, 0, count)
				for index := range count {
					all = append(all, map[string]any{"family": fmt.Sprintf("%064x", index), "label": fmt.Sprintf("Member %03d", index), "project": "example-project", "executable": "go", "measurements": map[string]any{"variantCount": 0, "successCount": 0, "incompleteCount": 0, "peakMemoryBytes": 0, "peakCpuCores": 0, "lastRunAt": nil}})
				}
				detail["members"] = all
				profile, _ := object(detail["profile"])
				profile["memberCount"] = count
				err = retainProfileMembers(detail)
			}
		case "profiles":
			var profiles map[string]map[string]any
			err = json.Unmarshal(value, &profiles)
			if err == nil {
				q.profiles = profiles
			}
		case "offline":
			err = json.Unmarshal(value, &q.offline)
		case "title":
			var title string
			err = json.Unmarshal(value, &title)
			if err == nil {
				q.jobs[1]["title"] = title
			}
		case "scheduler":
			var scheduler map[string]any
			err = json.Unmarshal(value, &scheduler)
			if err == nil {
				q.scheduler = scheduler
			}
		case "resources":
			var resources map[string]any
			err = json.Unmarshal(value, &resources)
			if err == nil {
				q.jobs[1]["resources"] = resources
			}
		default:
			err = errors.New("Unknown fixture state field")
		}
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (q *fakeQueue) screenshotData() error {
	now := time.Now().UTC().Truncate(time.Minute)
	examples := [][3]string{{"Build desktop app", "desktop-app", "feat/menu-bar"}, {"Run browser tests", "web-dashboard", "fix/queue-refresh"}, {"Typecheck API", "api-service", "feat/job-controls"}}
	for index, example := range examples {
		job := q.jobs[index]
		job["title"] = example[0]
		job["project"] = example[1]
		job["branch"] = example[2]
		job["submittedAt"] = now.Add(-time.Duration(5-index) * time.Minute).Format(time.RFC3339)
		if index == 0 {
			job["startedAt"] = now.Add(-2 * time.Minute).Format(time.RFC3339)
		}
		log, _ := job["logPath"].(string)
		if err := os.WriteFile(log, []byte("Starting "+example[0]+"\nExample validation step passed\n"), 0600); err != nil {
			return err
		}
	}
	return nil
}

// Run serves fake data until ctx ends. It returns its URL as one JSON line.
func Run(ctx context.Context, bind string, port int, screenshots bool, output io.Writer) error {
	ip := net.ParseIP(bind)
	if ip == nil || ip.To4() == nil || ip.IsUnspecified() || !ip.IsPrivate() && !ip.IsLoopback() {
		return errors.New("Use an explicit loopback or private IPv4 address")
	}
	if port < 0 || port > 65535 {
		return errors.New("Port must be from 0 to 65535")
	}
	root, err := os.MkdirTemp("", "agent-queue-browser-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	queue := &fakeQueue{root: root}
	if err := queue.reset(); err != nil {
		return err
	}
	if screenshots {
		if err := queue.screenshotData(); err != nil {
			return err
		}
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(bind, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer listener.Close()
	actualPort := listener.Addr().(*net.TCPAddr).Port
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	settings := remote.Settings{Hosts: []string{bind}, Port: actualPort, SecretKey: hex.EncodeToString(key)}
	options := remote.APIOptions{TestMode: true, Scheme: "http", InsecureCookie: true}
	if !screenshots {
		options.FixtureHandler = queue
	}
	handler, err := remote.NewHandler(settings, queue.run, options)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-done:
		}
	}()
	defer close(done)
	url := fmt.Sprintf("http://%s:%d", bind, actualPort)
	if screenshots {
		_, err = fmt.Fprintf(output, "Simulated dashboard: %s/\nAll projects and jobs are artificial. Ctrl+C stops this preview.\n", url)
	} else {
		err = json.NewEncoder(output).Encode(map[string]any{"url": url})
	}
	if err != nil {
		return err
	}
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Main runs the explicitly simulated dashboard server used by browser tests.
func Main(arguments []string) int {
	flags := flag.NewFlagSet("queue-preview", flag.ContinueOnError)
	bind := flags.String("bind", "127.0.0.1", "Explicit private or loopback IPv4 address")
	port := flags.Int("port", 0, "Port (0 selects an ephemeral port)")
	screenshots := flags.Bool("screenshots", false, "Use public screenshot examples and disable fixture mutation endpoints")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "Unexpected preview arguments")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := Run(ctx, *bind, *port, *screenshots, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
