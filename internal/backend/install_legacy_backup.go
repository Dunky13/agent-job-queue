package backend

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"

	"howett.net/plist"
)

// Automatic upgrades can retire incompatible options from a verified legacy
// service. Preserve its complete original privately before stopping that service
// or deleting its definition. The caller holds installation and scheduling locks
// and has already verified commands, loaded ownership and a drained queue.
func (install *installer) managedBackupLegacyDefinitions(plan *managedPlan) error {
	for label, expected := range plan.legacy {
		if label == "io.went.agent-queue" && plan.orphanedMenu {
			continue // Its historical cask already removed the definition.
		}
		filename := filepath.Join(install.launch, label+".plist")
		raw, err := updateReadOwned(filename, 16384)
		if err != nil {
			return err
		}
		var current Object
		if _, err := plist.Unmarshal(raw, &current); err != nil {
			return err
		}
		if !reflect.DeepEqual(current, expected) {
			return errors.New("Legacy service definition changed before backup; no service was stopped")
		}
		hash := sha256.Sum256(raw)
		backup := filepath.Join(install.config, "legacy-service-"+label+"-"+hex.EncodeToString(hash[:])+".plist")
		if retained, err := updateReadOwned(backup, 16384); err == nil {
			if !bytes.Equal(retained, raw) {
				return errors.New("The retained legacy service backup has changed")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		} else {
			parent, err := integrationParent(backup, false)
			if err != nil {
				return err
			}
			err = integrationPrivateWrite(parent, filepath.Base(backup), raw)
			if err == nil {
				err = parent.Sync()
			}
			parent.Close()
			if err != nil {
				return err
			}
		}
		latest, err := updateReadOwned(filename, 16384)
		if err != nil {
			return err
		}
		if !bytes.Equal(latest, raw) {
			return errors.New("Legacy service definition changed during backup; original retained and no service stopped")
		}
	}
	return nil
}
