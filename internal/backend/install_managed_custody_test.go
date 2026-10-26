package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestManagedSystemPublicationParentRequiresProtection(t *testing.T) {
	for _, info := range []unix.Stat_t{
		{Mode: unix.S_IFDIR | 0755, Uid: 0},
		{Mode: unix.S_IFDIR | 0500, Uid: 0},
		{Mode: unix.S_IFDIR | 0775, Uid: 0},
		{Mode: unix.S_IFDIR | 0777, Uid: 0},
		{Mode: unix.S_IFDIR | 0757, Uid: 0},
	} {
		err := managedSystemPublicationParentInfo(&info)
		if (err == nil) != (uint32(info.Mode)&0022 == 0) {
			t.Fatalf("mode %o: %v", info.Mode, err)
		}
		if err != nil && (!strings.Contains(err.Error(), "per-user Applications") || !strings.Contains(err.Error(), "approved protected location")) {
			t.Fatal("namespace error is not actionable", err)
		}
	}
}

func TestManagedWritablePublicationParentRefusedBeforeQuiescence(t *testing.T) {
	for _, bundle := range []bool{false, true} {
		t.Run(map[bool]string{false: "payload", true: "in-place"}[bundle], func(t *testing.T) {
			m := managedTestFixture(t)
			i := m.installer
			if err := m.run(t); err != nil {
				t.Fatal(err)
			}
			if bundle {
				i.appBundle, i.payload = true, filepath.Join(i.app, "Contents/Resources/payload")
			}
			parent := filepath.Dir(i.app)
			if err := os.Chmod(parent, 0775); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
			before := installTreeWitness(t, i.home)
			m.calls = nil
			if err := m.run(t); err == nil {
				t.Fatal("mutable publication parent was accepted")
			}
			managedRequireNoServiceMutation(t, m.calls)
			if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
				t.Fatal("namespace refusal mutated installation")
			}
		})
	}
}

func TestManagedUnownedBackupPreservedBeforeIdenticalReinstall(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(map[bool]string{false: "replacement", true: "identical"}[same], func(t *testing.T) {
			m := managedTestFixture(t)
			i := m.installer
			if err := m.run(t); err != nil {
				t.Fatal(err)
			}
			if err := installCopyTree(i.app, i.managedPreviousPath()); err != nil {
				t.Fatal(err)
			}
			if !same {
				managedCandidate(t, m, "124")
			}
			before := installTreeWitness(t, i.home)
			m.calls = nil
			if err := m.run(t); err == nil || !strings.Contains(err.Error(), "receipt") {
				t.Fatal("unowned backup accepted", err)
			}
			managedRequireNoServiceMutation(t, m.calls)
			if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
				t.Fatal("unowned backup refusal changed installation")
			}
		})
	}
}

func managedRetainedBackupFixture(t *testing.T) *managedFixture {
	t.Helper()
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	managedCandidate(t, m, "124")
	m.nativeFailure = "register resources"
	if err := m.run(t); err == nil {
		t.Fatal("expected partially published installation")
	}
	m.nativeFailure = ""
	return m
}

func TestManagedBackupSubstitutionRefusedOnRetryRecoveryAndCleanup(t *testing.T) {
	for _, operation := range []string{"retry", "recovery", "cleanup"} {
		for _, substitution := range []string{"same-bytes-new-inode", "changed-content", "symlink"} {
			t.Run(operation+"/"+substitution, func(t *testing.T) {
				m := managedRetainedBackupFixture(t)
				i := m.installer
				previous := i.managedPreviousPath()
				if operation == "recovery" {
					if err := os.Rename(i.app, i.app+".pending"); err != nil {
						t.Fatal(err)
					}
				}
				switch substitution {
				case "same-bytes-new-inode", "symlink":
					if err := os.Rename(previous, previous+".original"); err != nil {
						t.Fatal(err)
					}
					if substitution == "same-bytes-new-inode" {
						if err := installCopyTree(previous+".original", previous); err != nil {
							t.Fatal(err)
						}
					} else if err := os.Symlink(previous+".original", previous); err != nil {
						t.Fatal(err)
					}
				case "changed-content":
					integrationTestWrite(t, filepath.Join(previous, "Contents/Resources/unrelated"), []byte("preserve this"))
				}
				before := installTreeWitness(t, i.home)
				m.calls = nil
				var err error
				switch operation {
				case "retry":
					err = m.run(t)
				case "recovery":
					err = i.recoverManagedPublication(context.Background())
				case "cleanup":
					err = i.managedCleanupBackup()
				}
				if err == nil {
					t.Fatal("substituted backup accepted")
				}
				managedRequireNoServiceMutation(t, m.calls)
				if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
					t.Fatal("substitution refusal changed retained evidence")
				}
			})
		}
	}
}

func TestManagedPrivateCandidateFailureCleanupAndSubstitution(t *testing.T) {
	for _, failure := range []string{"signature", "changed-content", "substituted-root", "cancelled", "newer-target"} {
		t.Run(failure, func(t *testing.T) {
			m := managedTestFixture(t)
			i := m.installer
			if err := m.run(t); err != nil {
				t.Fatal(err)
			}
			managedCandidate(t, m, "124")
			original := i.command
			mutated := false
			var staged string
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			i.command = func(ctx context.Context, args []string) ([]byte, error) {
				last := args[len(args)-1]
				if !mutated && args[0] == "/usr/bin/codesign" && strings.Contains(last, ".Agent Queue.installing-") && strings.HasSuffix(last, ".app") {
					mutated, staged = true, last
					if !strings.HasPrefix(last, i.home+"/.AgentQueue-staging-") {
						t.Fatal("candidate signature verification escaped private staging", last)
					}
					switch failure {
					case "signature":
						return nil, errors.New("signature fixture refusal")
					case "changed-content":
						integrationTestWrite(t, filepath.Join(last, "Contents/Resources/substituted"), []byte("unexpected candidate"))
					case "substituted-root":
						if err := os.Rename(last, last+".original"); err != nil {
							t.Fatal(err)
						}
						if err := installCopyTree(last+".original", last); err != nil {
							t.Fatal(err)
						}
					case "cancelled":
						cancel()
					case "newer-target":
						integrationTestWrite(t, filepath.Join(i.app, "Contents/Resources/build-info.json"), mustMetadataJSON(t, metadataTestInfo(t, "125")))
					}
				}
				return original(ctx, args)
			}
			if err := i.execute(ctx); err == nil || !mutated {
				t.Fatal("missing staged failure", err, mutated)
			}
			workspaces, err := filepath.Glob(filepath.Join(i.home, ".AgentQueue-staging-*"))
			if err != nil {
				t.Fatal(err)
			}
			if failure == "substituted-root" {
				if len(workspaces) != 1 {
					t.Fatal("substituted candidate evidence was deleted", workspaces)
				}
				if _, err := os.Stat(staged); err != nil {
					t.Fatal("substituted candidate was not preserved", err)
				}
			} else if len(workspaces) != 0 {
				t.Fatal("owned failed staging leaked", workspaces)
			}
			publicStages, err := filepath.Glob(filepath.Join(filepath.Dir(i.app), ".Agent Queue.installing-*.app"))
			if err != nil || len(publicStages) != 0 {
				t.Fatal("owned public stage leaked", publicStages, err)
			}
		})
	}
}

func TestManagedRecoveryRechecksBackupAfterSignatureAndTargetBeforeRename(t *testing.T) {
	for _, substitution := range []string{"backup", "target"} {
		t.Run(substitution, func(t *testing.T) {
			m := managedRetainedBackupFixture(t)
			i := m.installer
			if err := os.Rename(i.app, i.app+".pending"); err != nil {
				t.Fatal(err)
			}
			previous := i.managedPreviousPath()
			mutated := false
			original := i.command
			i.command = func(ctx context.Context, args []string) ([]byte, error) {
				if !mutated && args[0] == "/usr/bin/codesign" && args[len(args)-1] == previous {
					mutated = true
					if substitution == "backup" {
						if err := os.Rename(previous, previous+".original"); err != nil {
							t.Fatal(err)
						}
						if err := installCopyTree(previous+".original", previous); err != nil {
							t.Fatal(err)
						}
					} else if err := installCopyTree(i.app+".pending", i.app); err != nil {
						t.Fatal(err)
					}
				}
				return original(ctx, args)
			}
			if err := i.recoverManagedPublication(context.Background()); err == nil || !mutated {
				t.Fatal("recovery accepted a substituted name during signature verification", err)
			}
			if _, err := os.Lstat(previous); err != nil {
				t.Fatal("recovery removed the retained backup", err)
			}
			if _, err := os.Lstat(i.managedReceiptPath()); err != nil {
				t.Fatal("recovery removed custody evidence", err)
			}
			if substitution == "target" {
				got, err := managedCaptureBundle(i.app)
				if err != nil || got.Build.Build == nil || *got.Build.Build != 124 {
					t.Fatal("recovery overwrote a concurrently appearing target", got, err)
				}
			}
		})
	}
}

func TestManagedOwnedBackupRecoveryRestoresThenRetriesExactCandidate(t *testing.T) {
	m := managedRetainedBackupFixture(t)
	i := m.installer
	if err := os.Rename(i.app, i.app+".pending"); err != nil {
		t.Fatal(err)
	}
	m.loaded = map[string]bool{}
	for _, name := range []string{"queue", "resources", "login"} {
		m.statuses[name] = "notRegistered"
	}
	if err := m.run(t); err != nil {
		t.Fatal("receipt-bound interrupted publication could not recover", err)
	}
	actual, err := managedCaptureBundle(i.app)
	if err != nil || actual.Build.Build == nil || *actual.Build.Build != 124 {
		t.Fatal("recovery did not finish the expected pending candidate", actual, err)
	}
	for _, filename := range []string{i.managedPreviousPath(), i.managedReceiptPath(), filepath.Join(i.root, "install-in-progress.json")} {
		if _, err := os.Lstat(filename); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("successful recovery retained owned publication state", filename, err)
		}
	}
}
