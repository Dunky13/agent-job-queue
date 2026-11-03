package backend

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Dunky13/agent-job-queue/internal/lifeline"
)

const usage = `Usage: agent-heavy <command>
  run [--label NAME] [--lane LANE] [--on-drain wait|fail|direct] -- COMMAND [ARGUMENTS...]
  status [--json]
  lane list|acquire|wait|status|renew|release [--json]
  plugins list|validate|reload [--json]
  pause|resume
  drain [--for DURATION] [--wait] [--cancel-queued] [--cancel-running]
  undrain [--force]
  daemon
  cancel|logs|start|next|last JOB_ID
  priority JOB_ID PRIORITY
  limit COUNT
  scheduling fixed|shadow|adaptive
  scheduler [--once]
  profiles list|show|edit|merge|combine|split|reset|regroup
  agents status|install|remove [--runner codex|claude] [--json]
  version [--json]
  updates check|install
Commands receive stdin EOF. Supply input as argv or an explicit file.
`

func Main(args []string) int {
	// First, before anything can spawn a process: see internal/lifeline.
	lifeline.Adopt(len(args) > 0 && (args[0] == "daemon" || args[0] == "scheduler"))
	syscall.Umask(0o077)
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	if commandHelp(args) {
		fmt.Print(usage)
		return 0
	}
	if args[0] == "--version" {
		args = append([]string{"version"}, args[1:]...)
	}
	switch args[0] {
	case "version", "updates":
		return UpdatesMain(args)
	case "profiles":
		return LearningMain(args[1:])
	case "agents":
		return IntegrationMain(args[1:])
	case "install":
		return InstallMain(args[1:])
	}
	if args[0] == "--" || args[0] == "--label" || args[0] == "--lane" || args[0] == "--on-drain" {
		args = append([]string{"run"}, args...)
	}
	g, err := NewGate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent-heavy:", err)
		return 125
	}
	code, err := dispatch(g, args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent-heavy:", err)
		return 125
	}
	return code
}

func commandHelp(args []string) bool {
	if len(args) < 2 {
		return false
	}
	if args[0] == "run" {
		for i := 1; i < len(args); i++ {
			switch {
			case args[i] == "--":
				return false
			case args[i] == "--label" || args[i] == "--lane" || args[i] == "--on-drain":
				i++
			case strings.HasPrefix(args[i], "--label=") || strings.HasPrefix(args[i], "--lane=") || strings.HasPrefix(args[i], "--on-drain="):
			case args[i] == "--help" || args[i] == "-h":
				return true
			default:
				return false
			}
		}
		return false
	}
	switch args[0] {
	case "daemon", "pause", "resume", "drain", "undrain", "execute-attempt", "status", "cancel", "logs", "cleanup", "execute", "next", "last", "priority", "limit", "start", "scheduling", "scheduler", "profiles", "agents", "version", "updates", "lane", "plugins":
		for _, arg := range args[1:] {
			if arg == "--help" || arg == "-h" {
				return true
			}
		}
	}
	return false
}
func argumentInt(value string) (int, error) {
	id, err := strconv.Atoi(value)
	if err != nil {
		return 0, errors.New("Expected an integer argument")
	}
	return id, nil
}
func runArguments(rest []string) ([]string, string, error) {
	argv, label, _, _, err := runLaneArguments(rest, false)
	return argv, label, err
}

// runLaneArguments also accepts --lane when lanes is true. Without it, a
// leading --lane stays part of the command as it always has. The fourth result
// is the --on-drain choice, wait by default.
func runLaneArguments(rest []string, lanes bool) ([]string, string, string, string, error) {
	label, lane, onDrain := "", "", drainWait
	for len(rest) > 0 {
		if rest[0] == "--" {
			return rest[1:], label, lane, onDrain, nil
		}
		if rest[0] == "--on-drain" || strings.HasPrefix(rest[0], "--on-drain=") {
			if rest[0] == "--on-drain" {
				if len(rest) < 2 {
					return nil, "", "", "", errDrainPolicy
				}
				onDrain, rest = rest[1], rest[2:]
			} else {
				onDrain, rest = strings.TrimPrefix(rest[0], "--on-drain="), rest[1:]
			}
			if onDrain != drainWait && onDrain != drainFail && onDrain != drainDirect {
				return nil, "", "", "", errDrainPolicy
			}
			continue
		}
		if lanes && (rest[0] == "--lane" || strings.HasPrefix(rest[0], "--lane=")) {
			if rest[0] == "--lane" {
				if len(rest) < 2 {
					return nil, "", "", "", errors.New("--lane requires a value")
				}
				lane, rest = rest[1], rest[2:]
			} else {
				lane, rest = strings.TrimPrefix(rest[0], "--lane="), rest[1:]
			}
			if lane != defaultLaneName && !pluginNamePattern.MatchString(lane) {
				return nil, "", "", "", errors.New("Lane names match ^[a-z][a-z0-9-]{0,31}$")
			}
			continue
		}
		if rest[0] == "--label" {
			if len(rest) < 2 {
				return nil, "", "", "", errors.New("--label requires a value")
			}
			label = rest[1]
			rest = rest[2:]
		} else if strings.HasPrefix(rest[0], "--label=") {
			label = strings.TrimPrefix(rest[0], "--label=")
			rest = rest[1:]
		} else {
			return rest, label, lane, onDrain, nil
		}
		if strings.TrimSpace(label) == "" {
			return nil, "", "", "", errors.New("Label must be 1 to 120 printable characters")
		}
	}
	return rest, label, lane, onDrain, nil
}
func dispatch(g *Gate, args []string) (int, error) {
	command := args[0]
	rest := args[1:]
	switch command {
	case "run":
		argv, label, lane, onDrain, err := runLaneArguments(rest, true)
		if err != nil {
			if lane != "" || containsString(rest, "--lane") || errors.Is(err, errDrainPolicy) {
				fmt.Fprintln(os.Stderr, "agent-heavy:", err)
				return laneExitUsage, nil
			}
			return 125, err
		}
		return RunInLaneOnDrain(g, argv, label, lane, onDrain)
	case "lane":
		return LaneMain(g, rest, os.Stdout, os.Stderr), nil
	case "plugins":
		return PluginsMain(g, rest, os.Stdout, os.Stderr), nil
	case "status":
		if len(rest) > 1 || (len(rest) == 1 && rest[0] != "--json") {
			return 125, errors.New("Usage: agent-heavy status [--json]")
		}
		data, err := StatusData(g)
		if err != nil {
			return 125, err
		}
		if len(rest) == 1 {
			return 0, json.NewEncoder(os.Stdout).Encode(data)
		}
		var limit any
		if data.Queue.Limit != nil {
			limit = *data.Queue.Limit
		}
		drainNote := ""
		if until, err := time.Parse(time.RFC3339, data.Queue.DrainUntil); err == nil {
			drainNote = ", draining until " + drainClock(until)
		} else if data.Queue.DrainError != "" {
			drainNote = ", intake closed by an invalid drain file"
		}
		fmt.Printf("Queue %s%s: %v running, %v waiting; limit %v\n", data.Queue.Status, drainNote, data.Queue.Running, data.Queue.Waiting, limit)
		fmt.Printf("Scheduling %s: %s\n", data.Scheduler.Mode, data.Scheduler.Reason)
		for _, job := range data.Jobs {
			context := job.Project
			if job.Branch != "" {
				context += " / " + job.Branch
			}
			suffix := ""
			if job.QueuePosition != nil {
				suffix += fmt.Sprintf("; queue #%v", *job.QueuePosition)
			}
			if job.ExitCode != nil {
				suffix += fmt.Sprintf("; exit=%v", *job.ExitCode)
			}
			if job.Kind == "lease" {
				fmt.Printf("  lease %v: %s | %s | lane %s%s\n", job.ID, job.Phase, job.Title, job.Lane, suffix)
				continue
			}
			if job.Lane != "" && job.Lane != defaultLaneName {
				suffix += "; lane " + job.Lane
			}
			fmt.Printf("  job %v: %s | %s | %s%s\n", job.ID, job.Phase, job.Title, context, suffix)
		}
		occupied := "no"
		if data.Queue.LegacyOccupied {
			occupied = "yes"
		}
		fmt.Println("Exclusive legacy owner:", occupied)
		return 0, nil
	case "daemon":
		if len(rest) != 0 {
			return 125, errors.New("Usage: agent-heavy daemon")
		}
		return 0, ServeQueue(g)
	case "drain":
		return DrainMain(g, rest)
	case "undrain":
		return UndrainMain(g, rest)
	case "pause", "resume":
		if len(rest) != 0 {
			return 125, errors.New("Usage: agent-heavy pause|resume")
		}
		_, err := g.QueueControl(command, nil)
		return 0, err
	case "execute-attempt":
		if len(rest) != 3 {
			return 125, errors.New("Invalid execution attempt arguments")
		}
		id, err := argumentInt(rest[0])
		if err != nil {
			return 125, err
		}
		return ExecuteAttempt(g, id, rest[1], rest[2])
	case "execute":
		return 125, errors.New("Legacy unfenced execution is unsupported; submit through run")
	case "scheduler":
		if len(rest) > 1 || (len(rest) == 1 && rest[0] != "--once") {
			return 125, errors.New("Usage: agent-heavy scheduler [--once]")
		}
		return 0, ServeScheduler(g, len(rest) == 1)
	case "scheduling":
		if len(rest) != 1 {
			return 125, errors.New("Usage: agent-heavy scheduling fixed|shadow|adaptive")
		}
		if err := SetSchedulingMode(g, rest[0]); err != nil {
			return 125, err
		}
		fmt.Printf("agent-heavy: scheduling mode %s saved\n", rest[0])
		return 0, nil
	case "priority":
		if len(rest) != 2 {
			return 125, errors.New("Expected two arguments")
		}
		first, err := argumentInt(rest[0])
		if err != nil {
			return 125, err
		}
		priority, err := argumentInt(rest[1])
		if err != nil {
			return 125, err
		}
		priority, err = Reorder(g, first, command, priority)
		if err == nil {
			fmt.Printf("agent-heavy: job %d priority %d\n", first, priority)
		}
		return 0, err
	case "cancel", "logs", "cleanup", "next", "last", "limit", "start":
		if len(rest) != 1 {
			return 125, errors.New("Expected one integer argument")
		}
		value, err := argumentInt(rest[0])
		if err != nil {
			return 125, err
		}
		if value < 0 {
			return 125, errors.New("Task ID must be nonnegative")
		}
		switch command {
		case "cancel":
			err = Cancel(g, value)
			if err == nil {
				fmt.Fprintf(os.Stderr, "agent-heavy: job %d cancelled\n", value)
			}
		case "logs":
			_, err = Drain(g.LogPath(value), 0)
		case "cleanup":
			err = Cleanup(g, value)
		case "next", "last":
			var priority int
			priority, err = Reorder(g, value, command, 0)
			if err == nil {
				fmt.Printf("agent-heavy: job %d priority %d\n", value, priority)
			}
		case "limit":
			err = SetLimit(g, value)
			if err == nil {
				fmt.Printf("agent-heavy: parallel limit %d saved\n", value)
			}
		case "start":
			err = StartJob(g, value)
			if err == nil {
				fmt.Printf("agent-heavy: job %d started now\n", value)
			}
		}
		return 0, err
	}
	return 125, fmt.Errorf("Unknown command %q; use --help", command)
}
