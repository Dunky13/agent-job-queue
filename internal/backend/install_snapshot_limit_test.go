package backend

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerLegacySnapshotRetainsLargeHistory(t *testing.T) {
	for _, kind := range []string{"retained-history", "oversized-history", "unrelated-output", "foreign-status"} {
		t.Run(kind, func(t *testing.T) {
			f := installTestFixture(t)
			f.legacy(t)
			padding := 5 * 1024 * 1024
			if kind == "oversized-history" {
				padding = 33 * 1024 * 1024
			}
			snapshot := f.snapshot()
			snapshot["tasks"] = Object{"7": Object{"id": 7, "command": strings.Repeat("x", padding), "status": Object{"Done": Object{"result": "Success"}}}}
			filename := filepath.Join(f.installer.home, "snapshot.json")
			integrationTestWrite(t, filename, mustMetadataJSON(t, snapshot))
			tool := filepath.Join(f.installer.path, "pueue")
			if err := os.WriteFile(tool, []byte("#!/bin/sh\nexec /bin/cat "+installQuote(filename)+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			f.installer.command = nil
			if kind == "unrelated-output" || kind == "foreign-status" {
				config, operation := filepath.Join(f.installer.config, "pueue.yml"), "log"
				if kind == "foreign-status" {
					config, operation = filepath.Join(f.installer.home, "foreign.yml"), "status"
				}
				if _, err := f.installer.run(context.Background(), "pueue", "--config", config, "--color", "never", operation, "--json"); err == nil {
					t.Fatal("unrelated control output lost its ordinary capture bound")
				}
				return
			}
			node, err := installConfig(filepath.Join(f.installer.config, "pueue.yml"))
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := f.installer.legacySnapshot(context.Background(), node, true)
			if kind == "oversized-history" {
				if err == nil {
					t.Fatal("oversized legacy history bypassed the retained-state bound")
				}
				return
			}
			if err != nil {
				t.Fatalf("retained queue history was truncated: %v", err)
			}
			if len(text(obj(obj(loaded["tasks"])["7"])["command"])) != padding {
				t.Fatal("retained task history changed during snapshot capture")
			}
		})
	}
}
