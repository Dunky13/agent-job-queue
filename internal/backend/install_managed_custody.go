package backend

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
	"golang.org/x/sys/unix"
	"howett.net/plist"
)

// A filename, a signature, or an open parent alone does not establish custody.
// Record both the exact filesystem object and its verified content identity in
// private installation state before moving the last good application.
type managedBundleIdentity struct {
	Device uint64    `json:"device"`
	Inode  uint64    `json:"inode"`
	Digest [32]byte  `json:"digest"`
	Build  BuildInfo `json:"build"`
}

type managedPublicationReceipt struct {
	SchemaVersion int                    `json:"schemaVersion"`
	App           string                 `json:"app"`
	Pending       BuildInfo              `json:"pending"`
	Candidate     managedBundleIdentity  `json:"candidate"`
	Backup        *managedBundleIdentity `json:"backup"`
}

func managedCaptureBundle(app string) (managedBundleIdentity, error) {
	var result managedBundleIdentity
	parent, err := installPayloadParent(app, filepath.Join(app, ".custody-probe"), true)
	if err != nil {
		return result, err
	}
	defer parent.Close()
	var before, after, named unix.Stat_t
	if err := unix.Fstat(int(parent.Fd()), &before); err != nil {
		return result, err
	}
	if err := installValidatePayloadTree(app, true); err != nil {
		return result, err
	}
	result.Device, result.Inode = uint64(before.Dev), before.Ino
	result.Digest, err = managedBundleDigest(app)
	if err != nil {
		return result, err
	}
	raw, err := installReadPayload(app, filepath.Join(app, "Contents/Info.plist"), 16384, true)
	if err != nil {
		return result, err
	}
	var info Object
	if _, err := plist.Unmarshal(raw, &info); err != nil {
		return result, err
	}
	files := []string{"Contents/Resources/build-info.json"}
	if info["AgentQueueServiceManagement"] == true {
		files = append(files, "Contents/Resources/payload/source/build-info.json")
	}
	for index, name := range files {
		raw, err := installReadPayload(app, filepath.Join(app, name), 16384, true)
		if err != nil {
			return result, err
		}
		build, err := ValidateBuildInfo(raw)
		if err != nil {
			return result, err
		}
		if index == 0 {
			result.Build = build
		} else if !result.Build.Equal(build) {
			return result, errors.New("Managed candidate and embedded support identities differ")
		}
	}
	build := "0"
	if result.Build.Build != nil {
		build = fmt.Sprint(*result.Build.Build)
	}
	if info["CFBundleIdentifier"] != "io.went.agent-queue" || info["CFBundleVersion"] != build || info["CFBundleShortVersionString"] != result.Build.Version {
		return result, errors.New("Managed candidate metadata differs from its expected build identity")
	}
	if err := unix.Fstat(int(parent.Fd()), &after); err != nil {
		return result, err
	}
	if err := unix.Lstat(app, &named); err != nil {
		return result, err
	}
	if integrationStatIdentity(&before) != integrationStatIdentity(&after) || named.Dev != before.Dev || named.Ino != before.Ino {
		return result, errors.New("Managed application changed while recording custody")
	}
	return result, nil
}

func (identity managedBundleIdentity) equal(other managedBundleIdentity) bool {
	return identity.Device == other.Device && identity.Inode == other.Inode && identity.Digest == other.Digest && identity.Build.Equal(other.Build)
}

func managedRequireBundle(app string, expected managedBundleIdentity) error {
	current, err := managedCaptureBundle(app)
	if err != nil {
		return err
	}
	if !expected.equal(current) {
		return errors.New("Managed application custody changed; substituted entries and recovery evidence preserved")
	}
	return nil
}

func (install *installer) managedReceiptPath() string {
	return filepath.Join(install.root, "publication-custody.json")
}

func (install *installer) managedPreviousPath() string {
	return filepath.Join(filepath.Dir(install.app), ".Agent Queue.previous.app")
}

func (install *installer) managedReadReceipt() (*managedPublicationReceipt, []byte, error) {
	raw, err := appservices.ReadFile(install.home, install.managedReceiptPath(), 16384, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	fields, err := metadataFields(raw, "schemaVersion", "app", "pending", "candidate", "backup")
	if err != nil {
		return nil, nil, err
	}
	if _, err := ValidateBuildInfo(fields["pending"]); err != nil {
		return nil, nil, err
	}
	for _, name := range []string{"candidate", "backup"} {
		if name == "backup" && string(fields[name]) == "null" {
			continue
		}
		bundle, err := metadataFields(fields[name], "device", "inode", "digest", "build")
		if err != nil {
			return nil, nil, err
		}
		if _, err := ValidateBuildInfo(bundle["build"]); err != nil {
			return nil, nil, err
		}
	}
	var receipt managedPublicationReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return nil, nil, err
	}
	if superseded, err := install.managedReceiptSuperseded(&receipt); err != nil {
		return nil, nil, err
	} else if superseded {
		return nil, nil, nil
	}
	if receipt.SchemaVersion != 1 || receipt.App != install.app || receipt.Candidate.Inode == 0 || receipt.Candidate.Digest == [32]byte{} || receipt.Backup != nil && (receipt.Backup.Inode == 0 || receipt.Backup.Digest == [32]byte{}) {
		return nil, nil, errors.New("Retained publication receipt belongs to another application; evidence preserved")
	}
	pending, err := installReadInfo(filepath.Join(install.root, "install-in-progress.json"))
	if err != nil || !pending.Equal(receipt.Pending) || !receipt.Pending.Equal(receipt.Candidate.Build) {
		return nil, nil, errors.New("Retained publication receipt differs from the pending installation; evidence preserved")
	}
	return &receipt, raw, nil
}

// A receipt guards an installer-made copy until its pending setup completes.
// When setup adopts a bundle the user placed with Finder, a candidate-only
// receipt without pending setup or a retained backup guards nothing.
func (install *installer) managedReceiptSuperseded(receipt *managedPublicationReceipt) (bool, error) {
	if !install.appBundle || install.prepareReplacement || install.unregisterServices || receipt.SchemaVersion != 1 || receipt.Backup != nil ||
		receipt.Candidate.Inode == 0 || receipt.Candidate.Digest == [32]byte{} {
		return false, nil
	}
	if pending, err := updateExists(filepath.Join(install.root, "install-in-progress.json")); err != nil || pending {
		return false, err
	}
	for _, app := range []string{install.app, receipt.App} {
		if !filepath.IsAbs(app) || filepath.Clean(app) != app {
			return false, nil
		}
		if retained, err := updateExists(filepath.Join(filepath.Dir(app), ".Agent Queue.previous.app")); err != nil || retained {
			return false, err
		}
	}
	return true, nil
}

// Retire a superseded receipt under the installation lock so final cleanup
// never compares the adopted bundle against an unrelated historical candidate.
func (install *installer) managedRetireSupersededReceipt() error {
	raw, err := appservices.ReadFile(install.home, install.managedReceiptPath(), 16384, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var receipt managedPublicationReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return err
	}
	if superseded, err := install.managedReceiptSuperseded(&receipt); err != nil || !superseded {
		return err
	}
	return install.managedRemoveFile(install.managedReceiptPath(), raw)
}

// This read-only check precedes all quiescence, including identical reinstall.
// A pre-existing backup without an exact private receipt is never adopted.
func (install *installer) managedBackupAccess() error {
	receipt, _, err := install.managedReadReceipt()
	if err != nil {
		return err
	}
	exists, err := updateExists(install.managedPreviousPath())
	if err != nil {
		return err
	}
	if exists {
		if receipt == nil || receipt.Backup == nil {
			return errors.New("A previous application has no owned publication receipt; preserve it and resolve the retained backup before retrying setup")
		}
		if err := managedRequireBundle(install.managedPreviousPath(), *receipt.Backup); err != nil {
			return err
		}
	}
	if receipt == nil {
		return nil
	}
	present, err := updateExists(install.app)
	if err != nil {
		return err
	}
	if !present {
		if exists {
			return nil // Recovery checks the same receipt under locks before rename.
		}
		return errors.New("Publication receipt has neither its candidate nor its backup; evidence preserved")
	}
	current, err := managedCaptureBundle(install.app)
	if err != nil {
		return err
	}
	if receipt.Candidate.equal(current) || !exists && receipt.Backup != nil && receipt.Backup.equal(current) {
		return nil // The latter is an interruption before the old app was moved.
	}
	return errors.New("Installed application differs from retained publication custody; evidence preserved")
}

// Preparation authorizes an external bundle move, which would invalidate an
// exact candidate receipt. Removal may retire candidate-only custody, but must
// preserve a retained last-good app until its pending publication is completed.
func (install *installer) managedLifecycleCustody() error {
	if !install.prepareReplacement && !install.unregisterServices {
		return nil
	}
	if err := install.managedBackupAccess(); err != nil {
		return err
	}
	receipt, _, err := install.managedReadReceipt()
	if err != nil || receipt == nil {
		return err
	}
	if install.prepareReplacement || receipt.Backup != nil {
		return errors.New("Complete the exact pending publication through explicit setup before preparing replacement or removing an app with a retained backup; preserve its receipt and application bundles")
	}
	return nil
}

func (install *installer) managedValidateExpected(expected BuildInfo) error {
	// validatePayload rechecks source, pending and target build ordering. Never
	// allow its identity refresh to replace the already selected candidate.
	err := install.validatePayload()
	current := install.identity
	install.identity = expected
	if err != nil {
		return err
	}
	if !current.Equal(expected) {
		return errors.New("Managed installer source changed from the expected pending build")
	}
	return nil
}

func (install *installer) managedCheckPublicationParent(parent *os.File) error {
	current, err := install.managedPublicationParent(true)
	if err != nil {
		return err
	}
	defer current.Close()
	var named, held unix.Stat_t
	if err := unix.Fstat(int(current.Fd()), &named); err != nil {
		return err
	}
	if err := unix.Fstat(int(parent.Fd()), &held); err != nil {
		return err
	}
	if named.Dev != held.Dev || named.Ino != held.Ino {
		return errors.New("Managed publication parent changed; evidence preserved")
	}
	return nil
}

func (install *installer) managedCleanupBackup() error {
	if err := install.managedBackupAccess(); err != nil {
		return err
	}
	receipt, raw, err := install.managedReadReceipt()
	if err != nil || receipt == nil {
		return err
	}
	if err := managedRequireBundle(install.app, receipt.Candidate); err != nil {
		return err
	}
	parent, err := install.managedPublicationParent(true)
	if err != nil {
		return err
	}
	defer parent.Close()
	if exists, err := updateExists(install.managedPreviousPath()); err != nil {
		return err
	} else if exists {
		if receipt.Backup == nil {
			return errors.New("Retained backup lacks owned custody; evidence preserved")
		}
		if err := managedRequireBundle(install.managedPreviousPath(), *receipt.Backup); err != nil {
			return err
		}
		root, err := os.OpenRoot(filepath.Dir(install.app))
		if err != nil {
			return err
		}
		err = root.RemoveAll(filepath.Base(install.managedPreviousPath()))
		root.Close()
		if err != nil {
			return err
		}
		if err := parent.Sync(); err != nil {
			return err
		}
	}
	return install.managedRemoveFile(install.managedReceiptPath(), raw)
}
