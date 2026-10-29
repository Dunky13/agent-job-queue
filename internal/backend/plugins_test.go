package backend

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pluginFixture(t *testing.T, g *Gate, name, manifest string, hooks map[string]string) string {
	t.Helper()
	directory := filepath.Join(pluginRoot(g), name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	for relative, body := range hooks {
		path := filepath.Join(directory, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

func laneManifest(plugin string, lanes ...string) string {
	return fmt.Sprintf(`{"schemaVersion":1,"name":%q,"lanes":[%s]}`, plugin, strings.Join(lanes, ","))
}

func pluginByName(registry pluginRegistry, name string) queuePlugin {
	for _, plugin := range registry.plugins {
		if plugin.Name == name {
			return plugin
		}
	}
	return queuePlugin{}
}

func TestPluginRegistryRejectsEveryEntryBeyondLimit(t *testing.T) {
	g := testGate(t)
	for index := range pluginLimit + 3 {
		name := fmt.Sprintf("plugin-%02d", index)
		pluginFixture(t, g, name, laneManifest(name, fmt.Sprintf(`{"name":%q,"concurrency":1}`, name)), nil)
	}
	registry := loadPluginRegistry(g)
	if len(registry.lanes) != pluginLimit {
		t.Errorf("loaded %d lanes, want %d", len(registry.lanes), pluginLimit)
	}
	for index := range pluginLimit + 3 {
		name := fmt.Sprintf("plugin-%02d", index)
		plugin := pluginByName(registry, name)
		if index < pluginLimit {
			if !plugin.Loaded || plugin.Error != "" {
				t.Errorf("plugin within limit refused: %+v", plugin)
			}
		} else if plugin.Loaded || plugin.Error == "" || len(plugin.Lanes) != 0 || len(plugin.Hooks) != 0 {
			t.Errorf("plugin beyond limit accepted: %+v", plugin)
		}
	}
}

func TestPluginManifestLoadsDefaultsAndHooks(t *testing.T) {
	g := testGate(t)
	pluginFixture(t, g, "qa-plugin", `{
  "schemaVersion": 1,
  "name": "qa-plugin",
  "description": "One QA session at a time",
  "lanes": [{"name": "qa", "label": "QA", "concurrency": 1, "priority": 50, "sharesLimit": false, "reservation": {"memoryMiB": 4096, "cpuPercent": 100}, "lease": {"ttlSeconds": 600}}, {"name": "qa-light", "concurrency": 2}],
  "hooks": {"lease.granted": ["hooks/granted"]}
}`, map[string]string{"hooks/granted": "#!/bin/sh\n"})
	registry := loadPluginRegistry(g)
	plugin := pluginByName(registry, "qa-plugin")
	if !plugin.Loaded || plugin.Error != "" || plugin.Warning != "" || len(plugin.Hooks["lease.granted"]) != 1 {
		t.Fatalf("plugin did not load: %+v", plugin)
	}
	qa := registry.lanes["qa"]
	if qa.Label != "QA" || qa.Concurrency != 1 || qa.Priority != 50 || qa.SharesLimit || qa.MemoryMiB != 4096 || qa.CPUPercent != 100 || qa.TTLSeconds != 600 || qa.Plugin != "qa-plugin" {
		t.Fatalf("lane fields: %+v", qa)
	}
	light := registry.lanes["qa-light"]
	if light.Label != "qa-light" || !light.SharesLimit || light.Priority != 0 || light.TTLSeconds != 0 {
		t.Fatalf("lane defaults: %+v", light)
	}
}

func TestPluginManifestStrictValidation(t *testing.T) {
	lane := `{"name":"qa","concurrency":1}`
	for name, manifest := range map[string]string{
		"unknown top-level field":  `{"schemaVersion":1,"name":"bad","lanes":[` + lane + `],"extra":true}`,
		"unknown lane field":       `{"schemaVersion":1,"name":"bad","lanes":[{"name":"qa","concurrency":1,"concurency":2}]}`,
		"case-folded field":        `{"schemaVersion":1,"Name":"bad","lanes":[` + lane + `]}`,
		"duplicate top-level key":  `{"schemaVersion":1,"name":"bad","name":"bad","lanes":[` + lane + `]}`,
		"duplicate lane key":       `{"schemaVersion":1,"name":"bad","lanes":[{"name":"qa","concurrency":1,"concurrency":8}]}`,
		"schema version":           `{"schemaVersion":2,"name":"bad","lanes":[` + lane + `]}`,
		"name mismatch":            `{"schemaVersion":1,"name":"other","lanes":[` + lane + `]}`,
		"reserved lane":            `{"schemaVersion":1,"name":"bad","lanes":[{"name":"default","concurrency":1}]}`,
		"concurrency range":        `{"schemaVersion":1,"name":"bad","lanes":[{"name":"qa","concurrency":9}]}`,
		"fractional concurrency":   `{"schemaVersion":1,"name":"bad","lanes":[{"name":"qa","concurrency":1.5}]}`,
		"null priority":            `{"schemaVersion":1,"name":"bad","lanes":[{"name":"qa","concurrency":1,"priority":null}]}`,
		"priority range":           `{"schemaVersion":1,"name":"bad","lanes":[{"name":"qa","concurrency":1,"priority":101}]}`,
		"ttl range":                `{"schemaVersion":1,"name":"bad","lanes":[{"name":"qa","concurrency":1,"lease":{"ttlSeconds":59}}]}`,
		"reservation range":        `{"schemaVersion":1,"name":"bad","lanes":[{"name":"qa","concurrency":1,"reservation":{"cpuPercent":801}}]}`,
		"label control character":  `{"schemaVersion":1,"name":"bad","lanes":[{"name":"qa","label":"Q\u0007A","concurrency":1}]}`,
		"duplicate lane in plugin": `{"schemaVersion":1,"name":"bad","lanes":[` + lane + `,` + lane + `]}`,
		"no lanes":                 `{"schemaVersion":1,"name":"bad","lanes":[]}`,
		"unknown hook event":       `{"schemaVersion":1,"name":"bad","lanes":[` + lane + `],"hooks":{"job.exploded":[]}}`,
		"trailing content":         `{"schemaVersion":1,"name":"bad","lanes":[` + lane + `]} {}`,
		"bad lane name":            `{"schemaVersion":1,"name":"bad","lanes":[{"name":"QA","concurrency":1}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			g := testGate(t)
			pluginFixture(t, g, "bad", manifest, nil)
			registry := loadPluginRegistry(g)
			plugin := pluginByName(registry, "bad")
			if plugin.Loaded || plugin.Error == "" || len(registry.lanes) != 0 {
				t.Fatalf("accepted invalid manifest: %+v %+v", plugin, registry.lanes)
			}
		})
	}
}

func TestPluginFileSafetyFailsClosed(t *testing.T) {
	manifest := laneManifest("unsafe", `{"name":"qa","concurrency":1}`)
	for name, prepare := range map[string]func(t *testing.T, directory string){
		"symlinked manifest": func(t *testing.T, directory string) {
			target := filepath.Join(t.TempDir(), "plugin.json")
			if err := os.WriteFile(target, []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			os.Remove(filepath.Join(directory, "plugin.json"))
			if err := os.Symlink(target, filepath.Join(directory, "plugin.json")); err != nil {
				t.Fatal(err)
			}
		},
		"group-writable manifest": func(t *testing.T, directory string) {
			if err := os.Chmod(filepath.Join(directory, "plugin.json"), 0o620); err != nil {
				t.Fatal(err)
			}
		},
		"world-writable plugin directory": func(t *testing.T, directory string) {
			if err := os.Chmod(directory, 0o757); err != nil {
				t.Fatal(err)
			}
		},
		"hard-linked manifest": func(t *testing.T, directory string) {
			if err := os.Link(filepath.Join(directory, "plugin.json"), filepath.Join(t.TempDir(), "alias")); err != nil {
				t.Skip("hard links unavailable across temporary directories")
			}
		},
		"oversized manifest": func(t *testing.T, directory string) {
			padded := strings.Replace(manifest, `"lanes"`, `"description":"`+strings.Repeat("x", 70*1024)+`","lanes"`, 1)
			if err := os.WriteFile(filepath.Join(directory, "plugin.json"), []byte(padded), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := testGate(t)
			directory := pluginFixture(t, g, "unsafe", manifest, nil)
			prepare(t, directory)
			registry := loadPluginRegistry(g)
			plugin := pluginByName(registry, "unsafe")
			if plugin.Loaded || plugin.Error == "" || len(registry.lanes) != 0 {
				t.Fatalf("loaded unsafe plugin: %+v", plugin)
			}
		})
	}
	t.Run("symlinked plugin directory", func(t *testing.T) {
		g := testGate(t)
		real := filepath.Join(t.TempDir(), "linked")
		if err := os.MkdirAll(real, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(real, "plugin.json"), []byte(laneManifest("linked", `{"name":"qa","concurrency":1}`)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(pluginRoot(g), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(pluginRoot(g), "linked")); err != nil {
			t.Fatal(err)
		}
		registry := loadPluginRegistry(g)
		if plugin := pluginByName(registry, "linked"); plugin.Loaded || plugin.Error == "" || len(registry.lanes) != 0 {
			t.Fatalf("followed a symlinked plugin: %+v", plugin)
		}
	})
	t.Run("unsafe plugins root", func(t *testing.T) {
		g := testGate(t)
		pluginFixture(t, g, "fine", laneManifest("fine", `{"name":"qa","concurrency":1}`), nil)
		if err := os.Chmod(pluginRoot(g), 0o777); err != nil {
			t.Fatal(err)
		}
		registry := loadPluginRegistry(g)
		if len(registry.lanes) != 0 || len(registry.plugins) != 1 || registry.plugins[0].Error == "" {
			t.Fatalf("loaded plugins below an unsafe root: %+v", registry)
		}
	})
	t.Run("misnamed directory", func(t *testing.T) {
		g := testGate(t)
		pluginFixture(t, g, "Bad_Name", laneManifest("Bad_Name", `{"name":"qa","concurrency":1}`), nil)
		registry := loadPluginRegistry(g)
		if len(registry.lanes) != 0 || len(registry.plugins) != 1 || registry.plugins[0].Loaded {
			t.Fatalf("loaded a misnamed plugin: %+v", registry)
		}
	})
}

func TestPluginLaneCollisionDisablesOnlyConflictingLanes(t *testing.T) {
	g := testGate(t)
	pluginFixture(t, g, "first", laneManifest("first", `{"name":"shared","concurrency":1}`, `{"name":"alpha","concurrency":1}`), nil)
	pluginFixture(t, g, "second", laneManifest("second", `{"name":"shared","concurrency":2}`, `{"name":"beta","concurrency":1}`), nil)
	pluginFixture(t, g, "third", laneManifest("third", `{"name":"gamma","concurrency":1}`), nil)
	registry := loadPluginRegistry(g)
	if _, ok := registry.lanes["shared"]; ok {
		t.Fatal("colliding lane stayed enabled")
	}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if _, ok := registry.lanes[name]; !ok {
			t.Fatalf("lane %s was disabled by an unrelated collision", name)
		}
	}
	for _, name := range []string{"first", "second"} {
		plugin := pluginByName(registry, name)
		if !plugin.Loaded || !strings.Contains(plugin.Error, "shared") {
			t.Fatalf("collision not reported for %s: %+v", name, plugin)
		}
	}
	if third := pluginByName(registry, "third"); third.Error != "" {
		t.Fatalf("unrelated plugin reported an error: %+v", third)
	}
}

func TestPluginHookSafetyRefusesUnsafeHooks(t *testing.T) {
	g := testGate(t)
	manifest := `{"schemaVersion":1,"name":"hooks","lanes":[{"name":"qa","concurrency":1}],"hooks":{
  "lease.granted":["hooks/good","hooks/linked","hooks/writable","hooks/plain"],
  "lease.released":["../escape","/bin/sh","hooks/../hooks/good","hooks/missing"]}}`
	directory := pluginFixture(t, g, "hooks", manifest, map[string]string{"hooks/good": "#!/bin/sh\n", "hooks/writable": "#!/bin/sh\n", "hooks/plain": "#!/bin/sh\n"})
	if err := os.Symlink("/bin/sh", filepath.Join(directory, "hooks", "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(directory, "hooks", "writable"), 0o772); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(directory, "hooks", "plain"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := loadPluginRegistry(g)
	plugin := pluginByName(registry, "hooks")
	if !plugin.Loaded || len(plugin.Hooks["lease.granted"]) != 1 || plugin.Hooks["lease.granted"][0] != "hooks/good" || len(plugin.Hooks["lease.released"]) != 0 {
		t.Fatalf("unsafe hooks accepted: %+v", plugin.Hooks)
	}
	if strings.Count(plugin.Warning, "hook skipped") != 7 {
		t.Fatalf("skipped hooks not reported: %q", plugin.Warning)
	}
	if _, ok := registry.lanes["qa"]; !ok {
		t.Fatal("an unsafe hook disabled the plugin's lane")
	}
	result, err := ValidatePluginDirectory(directory)
	if err != nil || result["valid"] != false || result["warning"] == nil {
		t.Fatalf("validate accepted unsafe hooks: %v %v", result, err)
	}
}

func TestValidatePluginDirectoryReportsDefinition(t *testing.T) {
	g := testGate(t)
	directory := pluginFixture(t, g, "valid", laneManifest("valid", `{"name":"qa","concurrency":2,"priority":-5}`), nil)
	result, err := ValidatePluginDirectory(directory)
	if err != nil || result["valid"] != true || result["error"] != nil {
		t.Fatalf("valid plugin rejected: %v %v", result, err)
	}
	lanes := result["lanes"].([]Object)
	if len(lanes) != 1 || lanes[0]["concurrency"] != 2 || lanes[0]["priority"] != -5 || lanes[0]["sharesLimit"] != true {
		t.Fatalf("lane definition: %v", lanes)
	}
}
