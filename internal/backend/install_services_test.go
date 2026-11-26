package backend

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// memoryServices is the installer's service port for tests. loaded states which
// labels the login session holds, output what printing a loaded one reports.
// Every operation is recorded as {"service", operation, label or definition}.
type memoryServices struct {
	loaded *map[string]bool
	output func(label string) []byte
	// print answers Print; a test wraps it to state other output or a failure.
	print func(label string) ([]byte, error)
	// bootout runs before a service is unloaded; a test wraps it to act then.
	bootout func(label string)
	record  func(call []string) error
}

func newMemoryServices(loaded *map[string]bool, record func([]string) error) *memoryServices {
	services := &memoryServices{loaded: loaded, record: record, bootout: func(string) {}}
	services.output = func(string) []byte { return nil }
	services.print = func(label string) ([]byte, error) {
		if !(*services.loaded)[label] {
			return nil, errInstallServiceNotLoaded
		}
		return services.output(label), nil
	}
	return services
}

func (services *memoryServices) Print(_ context.Context, label string) ([]byte, error) {
	if err := services.record([]string{"service", "print", label}); err != nil {
		return nil, err
	}
	return services.print(label)
}

func (services *memoryServices) Bootout(_ context.Context, label string) error {
	if err := services.record([]string{"service", "bootout", label}); err != nil {
		return err
	}
	services.bootout(label)
	delete(*services.loaded, label)
	return nil
}

func (services *memoryServices) Bootstrap(_ context.Context, definition string) error {
	if err := services.record([]string{"service", "bootstrap", definition}); err != nil {
		return err
	}
	(*services.loaded)[strings.TrimSuffix(filepath.Base(definition), ".plist")] = true
	return nil
}

// The launchctl adapter's own seam is the installer's command runner.
func TestInstallLaunchctlServices(t *testing.T) {
	domain := "gui/" + strconv.Itoa(os.Getuid())
	exit := exec.Command("/bin/sh", "-c", "exit 113").Run()
	if exit == nil {
		t.Fatal("fixture did not produce the absent-service exit")
	}
	var calls [][]string
	var output []byte
	var failure error
	install := &installer{command: func(_ context.Context, args []string) ([]byte, error) {
		calls = append(calls, slices.Clone(args))
		return output, failure
	}}
	services, ctx := install.loginServices(), context.Background()

	label := "io.went.agent-heavy"
	output = []byte("program = /helper\n")
	if got, err := services.Print(ctx, label); err != nil || string(got) != string(output) {
		t.Fatalf("loaded service: %q %v", got, err)
	}
	absent := func(label string) []byte {
		return []byte("Bad request.\nCould not find service \"" + label + "\" in domain for user gui: " + strconv.Itoa(os.Getuid()) + "\n")
	}
	output, failure = absent(label), exit
	if got, err := services.Print(ctx, label); got != nil || !errors.Is(err, errInstallServiceNotLoaded) {
		t.Fatalf("absent service: %q %v", got, err)
	}
	for _, witness := range []struct {
		label  string
		output []byte
	}{
		{label, []byte("Operation not permitted\n")},
		{"foreign.service", absent("foreign.service")},
	} {
		output = witness.output
		got, err := services.Print(ctx, witness.label)
		if got != nil || !errors.Is(err, exit) || errors.Is(err, errInstallServiceNotLoaded) {
			t.Fatalf("ambiguous absence of %s: %q %v", witness.label, got, err)
		}
	}
	output, failure = nil, nil
	if err := services.Bootout(ctx, label); err != nil {
		t.Fatal(err)
	}
	if err := services.Bootstrap(ctx, "/definitions/"+label+".plist"); err != nil {
		t.Fatal(err)
	}
	failure = exit
	if err := services.Bootout(ctx, label); !errors.Is(err, exit) {
		t.Fatal("bootout hid its failure", err)
	}
	want := [][]string{
		{"launchctl", "print", domain + "/" + label},
		{"launchctl", "print", domain + "/" + label},
		{"launchctl", "print", domain + "/" + label},
		{"launchctl", "print", domain + "/foreign.service"},
		{"launchctl", "bootout", domain + "/" + label},
		{"launchctl", "bootstrap", domain, "/definitions/" + label + ".plist"},
		{"launchctl", "bootout", domain + "/" + label},
	}
	if !slices.EqualFunc(calls, want, slices.Equal[[]string]) {
		t.Fatalf("launchctl argv: %v", calls)
	}
}
