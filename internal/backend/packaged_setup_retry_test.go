package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPackagedSetupRetriesOnlyUnacceptedKernelBusy(t *testing.T) {
	f := installTestFixture(t)
	f.calls = append(f.calls, []string{"AgentQueue", "--service-management", "status", "queue"})
	ctx := context.Background()
	calls := 0
	retries, err := runPackagedSetup(ctx, f, func(received context.Context) error {
		if received != ctx {
			t.Fatal("retry changed the caller's operation budget")
		}
		calls++
		if calls == 1 {
			f.calls = append(f.calls, []string{"launchctl", "print", "private-read"})
			return fmt.Errorf("private scheduling lock: %w", syscall.EWOULDBLOCK)
		}
		return nil
	})
	if err != nil || retries != 1 || calls != 2 {
		t.Fatal("safe refusal did not resume exactly once", retries, calls, err)
	}
}

func TestPackagedSetupNeverRetriesMutationOrUncertainHistory(t *testing.T) {
	for _, phase := range []string{"existing-pending", "new-pending", "metadata", "wrapper", "same-content-replacement", "native", "menu", "launchctl", "lost-history", "changed-history"} {
		t.Run(phase, func(t *testing.T) {
			f := installTestFixture(t)
			i := f.installer
			f.calls = append(f.calls, []string{"AgentQueue", "--service-management", "status", "queue"})
			pending := filepath.Join(i.root, "install-in-progress.json")
			metadata := filepath.Join(i.root, "installation.json")
			if phase == "existing-pending" {
				integrationTestWrite(t, pending, []byte("retained pending identity"))
			}
			if phase == "same-content-replacement" {
				integrationTestWrite(t, metadata, []byte("retained identity"))
			}
			calls := 0
			retries, err := runPackagedSetup(context.Background(), f, func(context.Context) error {
				calls++
				switch phase {
				case "new-pending":
					integrationTestWrite(t, pending, []byte("accepted identity"))
				case "metadata":
					integrationTestWrite(t, metadata, []byte("changed identity"))
				case "wrapper":
					integrationTestWrite(t, filepath.Join(i.home, ".local/bin/agent-heavy"), []byte("changed wrapper"))
				case "same-content-replacement":
					if err := os.Rename(metadata, metadata+".retained"); err != nil {
						t.Fatal(err)
					}
					integrationTestWrite(t, metadata, []byte("retained identity"))
				case "native":
					f.calls = append(f.calls, []string{"AgentQueue", "--service-management", "unregister", "resources"})
				case "menu":
					f.calls = append(f.calls, []string{"AgentQueue", "--prepare-app-replacement", i.app})
				case "launchctl":
					f.calls = append(f.calls, []string{"launchctl", "bootout", "private-service"})
				case "lost-history":
					f.calls = nil
				case "changed-history":
					f.calls[0][1] = "changed"
				}
				return syscall.EAGAIN
			})
			if err == nil || retries != 0 || calls != 1 {
				t.Fatal("accepted or uncertain operation was repeated", retries, calls, err)
			}
		})
	}
}

func TestPackagedSetupPreservesFailureBoundsAndCancellation(t *testing.T) {
	for _, failure := range []error{io.EOF, errors.New("Another installer or queue change is running")} {
		f := installTestFixture(t)
		calls := 0
		_, err := runPackagedSetup(context.Background(), f, func(context.Context) error { calls++; return failure })
		if !errors.Is(err, failure) || calls != 1 {
			t.Fatal("non-kernel failure was repeated or replaced", calls, err)
		}
	}
	f := installTestFixture(t)
	calls := 0
	retries, err := runPackagedSetup(context.Background(), f, func(context.Context) error { calls++; return syscall.EWOULDBLOCK })
	if err == nil || calls != 3 || retries != 2 {
		t.Fatal("busy attempts escaped their bound", retries, calls, err)
	}
	for _, alreadyCancelled := range []bool{false, true} {
		f := installTestFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		if alreadyCancelled {
			cancel()
		}
		_, err := runPackagedSetup(ctx, f, func(context.Context) error { calls++; cancel(); return syscall.EWOULDBLOCK })
		if !errors.Is(err, context.Canceled) || calls > 1 || alreadyCancelled && calls != 0 {
			t.Fatal("cancelled setup was retried", calls, err)
		}
	}
}
