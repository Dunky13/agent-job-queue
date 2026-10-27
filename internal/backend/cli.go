package backend

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

const usage = `Usage: agent-heavy <command>
  run [--label NAME] -- COMMAND [ARGUMENTS...]
  status [--json]
  pause|resume
  daemon
  cancel|logs|start|next|last JOB_ID
  priority JOB_ID PRIORITY
  limit COUNT
  scheduling fixed|shadow|adaptive
  scheduler [--once]
  profiles list|show|edit|merge|split|reset
  agents status|install|remove [--runner codex|claude] [--json]
  version [--json]
  handover-capabilities --json
  updates check|install
Commands receive stdin EOF. Supply input as argv or an explicit file.
`

func Main(args []string) int {
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
	case "handover-capabilities":
		if len(args) != 2 || args[1] != "--json" {
			fmt.Fprintln(os.Stderr, "Usage: agent-heavy handover-capabilities --json")
			return 2
		}
		if err := json.NewEncoder(os.Stdout).Encode(Object{"schemaVersion": 1, "protocol": queueHandoverProtocol, "queueSchema": queueHandoverSchema}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case "version", "updates":
		return UpdatesMain(args)
	case "profiles":
		return LearningMain(args[1:])
	case "agents":
		return IntegrationMain(args[1:])
	case "install":
		return InstallMain(args[1:])
	}
	if args[0] == "--" || args[0] == "--label" {
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
			case args[i] == "--label":
				i++
			case strings.HasPrefix(args[i], "--label="):
			case args[i] == "--help" || args[i] == "-h":
				return true
			default:
				return false
			}
		}
		return false
	}
	switch args[0] {
	case "daemon", "pause", "resume", "execute-attempt", "status", "cancel", "logs", "cleanup", "execute", "next", "last", "priority", "limit", "start", "scheduling", "scheduler", "profiles", "agents", "version", "updates":
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
	label := ""
	for len(rest) > 0 {
		if rest[0] == "--" {
			return rest[1:], label, nil
		}
		if rest[0] == "--label" {
			if len(rest) < 2 {
				return nil, "", errors.New("--label requires a value")
			}
			label = rest[1]
			rest = rest[2:]
		} else if strings.HasPrefix(rest[0], "--label=") {
			label = strings.TrimPrefix(rest[0], "--label=")
			rest = rest[1:]
		} else {
			return rest, label, nil
		}
		if strings.TrimSpace(label) == "" {
			return nil, "", errors.New("Label must be 1 to 120 printable characters")
		}
	}
	return rest, label, nil
}
func dispatch(g *Gate, args []string) (int, error) {
	command := args[0]
	rest := args[1:]
	switch command {
	case "run":
		argv, label, err := runArguments(rest)
		if err != nil {
			return 125, err
		}
		return Run(g, argv, label)
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
		queue := obj(data["queue"])
		scheduler := obj(data["scheduler"])
		fmt.Printf("Queue %s: %v running, %v waiting; limit %v\n", text(queue["status"]), queue["running"], queue["waiting"], queue["limit"])
		fmt.Printf("Scheduling %s: %s\n", text(scheduler["mode"]), text(scheduler["reason"]))
		for _, raw := range list(data["jobs"]) {
			job := obj(raw)
			context := text(job["project"])
			if text(job["branch"]) != "" {
				context += " / " + text(job["branch"])
			}
			suffix := ""
			if job["queuePosition"] != nil {
				suffix += fmt.Sprintf("; queue #%v", job["queuePosition"])
			}
			if job["exitCode"] != nil {
				suffix += fmt.Sprintf("; exit=%v", job["exitCode"])
			}
			fmt.Printf("  job %v: %s | %s | %s%s\n", job["id"], text(job["phase"]), text(job["title"]), context, suffix)
		}
		occupied := "no"
		if boolean(queue["legacyOccupied"]) {
			occupied = "yes"
		}
		fmt.Println("Exclusive legacy owner:", occupied)
		return 0, nil
	case "daemon":
		if len(rest) != 0 {
			return 125, errors.New("Usage: agent-heavy daemon")
		}
		return 0, ServeQueue(g)
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
