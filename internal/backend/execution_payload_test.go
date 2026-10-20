package backend

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestJobPayloadPreservesUnixBytesAndLegacyJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload.json")
	argv := []string{"/bin/sh", "", "arg-\xff"}
	cwd := "/directory-\xfe"
	environment := Object{"BYTES": "value-\xfd", "EMPTY": "", "NAME-\xfc": "name-value-\xfb"}
	content := Object{"token": token(), "argv": argv, "cwd": cwd, "env": environment, "task_id": 42, "resource_managed": true}
	if err := WriteJobPayload(path, content); err != nil {
		t.Fatal(err)
	}
	for rewrite := 0; rewrite < 2; rewrite++ {
		decoded, err := ReadJobPayload(path)
		if err != nil {
			t.Fatal(err)
		}
		gotArgv, gotCwd, gotEnvironment, err := executionPayloadFields(decoded)
		if err != nil || gotCwd != cwd || gotArgv[2] != argv[2] || gotArgv[1] != "" || gotEnvironment["BYTES"] != "value-\xfd" || gotEnvironment["EMPTY"] != "" || gotEnvironment["NAME-\xfc"] != "name-value-\xfb" || decoded["resource_managed"] != true {
			t.Fatalf("execution payload changed bytes/metadata: argv=%q cwd=%q environment=%q err=%v", gotArgv, gotCwd, gotEnvironment, err)
		}
		if err := WriteJobPayload(path, decoded); err != nil {
			t.Fatal(err)
		}
	}
	wire, err := ReadPrivate(path)
	if err != nil || wire["argv"] != nil || wire["cwd"] != nil || wire["env"] != nil || obj(wire["execution_bytes"]) == nil {
		t.Fatal("private encoding did not isolate execution bytes", wire, err)
	}
	legacy := Object{"token": token(), "argv": []string{"/bin/sh", "-c", "echo café 🙂"}, "cwd": "/fixture", "env": Object{"VALUE": "é"}, "task_id": 3}
	mustWrite(t, path, legacy)
	decoded, err := ReadJobPayload(path)
	if err != nil || !jsonEqual(decoded, legacy) {
		t.Fatalf("legacy JSON compatibility changed: %v %v", decoded, err)
	}
}

func validBytePayload() Object {
	return Object{"version": 1, "argv": []string{base64.StdEncoding.EncodeToString([]byte("/bin/sh"))}, "cwd": base64.StdEncoding.EncodeToString([]byte("/fixture")), "env": []Object{{"name": "VkFMVUU=", "value": "dGV4dA=="}}}
}

func TestJobPayloadRejectsMalformedByteSchema(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(Object) Object
	}{
		{"unknown-version", func(v Object) Object { v["version"] = 2; return v }},
		{"missing-version", func(v Object) Object { delete(v, "version"); return v }},
		{"capitalized-version", func(v Object) Object { delete(v, "version"); v["Version"] = 1; return v }},
		{"unknown-field", func(v Object) Object { v["extra"] = true; return v }},
		{"null-schema", func(Object) Object { return nil }},
		{"null-argv", func(v Object) Object { v["argv"] = nil; return v }},
		{"empty-argv", func(v Object) Object { v["argv"] = []string{}; return v }},
		{"null-argument", func(v Object) Object { v["argv"] = []any{nil}; return v }},
		{"array-argument", func(v Object) Object { v["argv"] = []any{[]int{65}}; return v }},
		{"invalid-base64-argument", func(v Object) Object { v["argv"] = []string{"!invalid!"}; return v }},
		{"nul-argument", func(v Object) Object { v["argv"] = []string{"AA=="}; return v }},
		{"null-cwd", func(v Object) Object { v["cwd"] = nil; return v }},
		{"array-cwd", func(v Object) Object { v["cwd"] = []int{65}; return v }},
		{"nul-cwd", func(v Object) Object { v["cwd"] = "AA=="; return v }},
		{"null-environment", func(v Object) Object { v["env"] = nil; return v }},
		{"object-environment", func(v Object) Object { v["env"] = Object{"VALUE": "dGV4dA=="}; return v }},
		{"null-environment-entry", func(v Object) Object { v["env"] = []any{nil}; return v }},
		{"missing-environment-name", func(v Object) Object { v["env"] = []Object{{"value": "dGV4dA=="}}; return v }},
		{"invalid-base64-environment-name", func(v Object) Object { v["env"] = []Object{{"name": "!invalid!", "value": "dGV4dA=="}}; return v }},
		{"nul-environment-name", func(v Object) Object { v["env"] = []Object{{"name": "AA==", "value": "dGV4dA=="}}; return v }},
		{"duplicate-environment-name", func(v Object) Object {
			v["env"] = []Object{{"name": "VkFMVUU=", "value": "dGV4dA=="}, {"name": "VkFMVUU=", "value": "b3RoZXI="}}
			return v
		}},
		{"array-environment-value", func(v Object) Object { v["env"] = []Object{{"name": "VkFMVUU=", "value": []int{65}}}; return v }},
		{"null-environment-value", func(v Object) Object { v["env"] = []Object{{"name": "VkFMVUU=", "value": nil}}; return v }},
		{"invalid-base64-environment", func(v Object) Object { v["env"] = []Object{{"name": "VkFMVUU=", "value": "!invalid!"}}; return v }},
		{"nul-environment", func(v Object) Object { v["env"] = []Object{{"name": "VkFMVUU=", "value": "AA=="}}; return v }},
		{"invalid-environment-name", func(v Object) Object {
			v["env"] = []Object{{"name": base64.StdEncoding.EncodeToString([]byte("INVALID=NAME")), "value": "dGV4dA=="}}
			return v
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "payload.json")
			mustWrite(t, path, Object{"execution_bytes": test.change(validBytePayload())})
			if _, err := ReadJobPayload(path); err == nil {
				t.Fatal("accepted malformed private execution bytes")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "payload.json")
	for _, key := range []string{"argv", "cwd", "env"} {
		mustWrite(t, path, Object{"execution_bytes": validBytePayload(), key: nil})
		if _, err := ReadJobPayload(path); err == nil {
			t.Fatal("accepted mixed legacy and byte payload", key)
		}
	}
}

func TestJobPayloadRetainsPrivateOwnershipChecks(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "payload.json")
	content := Object{"argv": []string{"/bin/sh"}, "cwd": directory, "env": Object{}}
	if err := WriteJobPayload(path, content); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadJobPayload(link); err == nil {
		t.Fatal("read execution bytes through symlink")
	}
	if err := WriteJobPayload(link, content); err == nil {
		t.Fatal("replaced execution payload symlink")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadJobPayload(path); err == nil {
		t.Fatal("read public execution bytes")
	}
	if err := WriteJobPayload(path, content); err == nil {
		t.Fatal("replaced unsafe execution payload")
	}
}

func TestJobPayloadSchedulingAndDescriptionConsumers(t *testing.T) {
	g := testGate(t)
	value := token()
	path, _ := g.PayloadPath(value)
	argv := []string{"/bin/sh", "-c", "exit 0", "arg-\xff"}
	environment := map[string]string{"VALUE": "env-\xfe"}
	if err := WriteJobPayload(path, Object{"token": value, "task_id": 0, "argv": argv, "cwd": g.State, "env": environment, "label": "Validation batch", "explicit_label": false}); err != nil {
		t.Fatal(err)
	}
	expected := identityForTest(t, argv, g.State, environment)
	identity, err := EnsureSchedulingIdentity(g, value, 0)
	if err != nil || !jsonEqual(identity, expected) {
		t.Fatalf("scheduler did not recover the same raw-byte identity: got=%v expected=%v err=%v", identity, expected, err)
	}
	description, err := DescribeTask(g, Object{"token": value, "path": g.State, "label": "Validation batch"}, "queued")
	if err != nil || description["title"] != "Validation batch" || description["executable"] != "sh" {
		t.Fatalf("metadata fallback did not decode private byte payload: %v %v", description, err)
	}
}
