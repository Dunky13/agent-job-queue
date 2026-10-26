package backend

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// Only native lifecycle and OS ownership readback are simulated. Readiness and
// drain snapshots come from the actual packaged canonical helper and private DB.
// The parent test owns all processes, independently of installer command scopes.
func managedPackagedRuntimeFixture(t *testing.T, m *managedFixture) {
	t.Helper()
	i := m.installer
	type process struct {
		command *exec.Cmd
		done    chan error
		log     *os.File
	}
	processes := map[string]*process{}
	stop := func(service string) {
		t.Helper()
		p := processes[service]
		if p == nil {
			return
		}
		delete(processes, service)
		_ = p.command.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			_ = p.command.Process.Kill()
			<-p.done
			t.Errorf("packaged %s required forced teardown", service)
		}
		if err := p.log.Close(); err != nil {
			t.Error(err)
		}
		if err := p.command.Process.Signal(syscall.Signal(0)); !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("packaged %s was not reaped: %v", service, err)
		}
	}
	t.Cleanup(func() { stop("resources"); stop("queue") })
	start := func(service string) error {
		if processes[service] != nil {
			return nil
		}
		name, argument := "daemon", "daemon"
		if service == "resources" {
			name, argument = "scheduler", "scheduler"
		}
		log, err := os.Create(filepath.Join(i.home, name+".log"))
		if err != nil {
			return err
		}
		command := exec.Command(filepath.Join(i.app, "Contents/MacOS/agent-heavy"), argument)
		command.Env = i.commandEnvironment([]string{filepath.Join(i.app, "Contents/MacOS/agent-heavy"), "status", "--json"})
		command.Stdout, command.Stderr = log, log
		if err := command.Start(); err != nil {
			log.Close()
			return err
		}
		p := &process{command: command, done: make(chan error, 1), log: log}
		processes[service] = p
		go func() { p.done <- command.Wait() }()
		return nil
	}
	heavy := filepath.Join(i.app, "Contents/MacOS/agent-heavy")
	status := func(ctx context.Context) ([]byte, error) {
		command := exec.CommandContext(ctx, heavy, "status", "--json")
		command.Env = i.commandEnvironment([]string{heavy, "status", "--json"})
		output, err := command.CombinedOutput()
		witnessErr := os.WriteFile(filepath.Join(i.home, "readiness.json"), output, 0600)
		return output, errors.Join(err, witnessErr)
	}
	// Use the production private RPC snapshot, not the fixture's synthetic tasks
	// or the public status CLI's different JSON schema.
	i.queueSnapshot = func(ctx context.Context) (Object, error) {
		deadline := time.Now().Add(15 * time.Second)
		if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
			deadline = limit
		}
		gate := &Gate{State: i.state, Config: filepath.Join(i.config, "pueue.yml"),
			ControlEnv: map[string]string{"HOME": i.home, "AGENT_HEAVY_STATE": i.state}, ControlDeadline: deadline}
		return gate.Snapshot()
	}
	original := i.command
	i.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 3 && args[1] == "--prepare-app-replacement" && args[2] == i.app &&
			(args[0] == filepath.Join(i.app, "Contents/MacOS/AgentQueue") || args[0] == filepath.Join(i.payload, "Agent Queue.app/Contents/MacOS/AgentQueue")) {
			m.calls = append(m.calls, slices.Clone(args))
			return mustMetadataJSON(t, Object{"schemaVersion": 1, "app": i.app, "status": "stopped"}), nil
		}
		if slices.Equal(args, []string{heavy, "status", "--json"}) {
			m.calls = append(m.calls, slices.Clone(args))
			return status(ctx)
		}
		if len(args) == 4 && args[1] == "--service-management" && (args[3] == "queue" || args[3] == "resources") {
			if args[2] == "unregister" {
				stop(args[3])
			}
			output, err := original(ctx, args)
			if err == nil && args[2] == "register" && m.statuses[args[3]] == "enabled" {
				err = start(args[3])
			}
			return output, err
		}
		return original(ctx, args)
	}
}
