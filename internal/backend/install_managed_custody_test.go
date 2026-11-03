package backend

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
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
		err := managedSystemPublicationParentInfo(&info, false)
		if (err == nil) != (uint32(info.Mode)&0022 == 0) {
			t.Fatalf("mode %o: %v", info.Mode, err)
		}
		admin := info
		admin.Gid = appservices.AdminGroup
		if stock := managedSystemPublicationParentInfo(&admin, true); (stock == nil) != (uint32(info.Mode)&0002 == 0) {
			t.Fatalf("stock Applications mode %o: %v", info.Mode, stock)
		}
		if managedSystemPublicationParentInfo(&admin, false) != nil && err == nil {
			t.Fatalf("admin group changed the root decision for mode %o", info.Mode)
		}
		if err != nil && (!strings.Contains(err.Error(), "per-user Applications") || !strings.Contains(err.Error(), "approved protected location")) {
			t.Fatal("namespace error is not actionable", err)
		}
	}
}

func TestManagedWritablePublicationParentRefusedBeforeQuiescence(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	if err := m.run(t); err != nil {
		t.Fatal(err)
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
	managedOlderPublication(t, m, "124")
	return m
}

func TestManagedBackupSubstitutionRefusedOnRetryAndCleanup(t *testing.T) {
	for _, operation := range []string{"retry", "cleanup"} {
		for _, substitution := range []string{"same-bytes-new-inode", "changed-content", "symlink"} {
			t.Run(operation+"/"+substitution, func(t *testing.T) {
				m := managedRetainedBackupFixture(t)
				i := m.installer
				previous := i.managedPreviousPath()
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
