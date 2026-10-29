package backend

import (
	"encoding/json"
	"strings"
	"testing"
)

const metadataTestCommit = "abcdef0" + "111111111111111111111111111111111"

func metadataTestInfo(t *testing.T, build string) BuildInfo {
	t.Helper()
	environment := map[string]string{}
	if build != "" {
		environment["BUILD_NUMBER"] = build
		environment["SOURCE_COMMIT"] = metadataTestCommit
	}
	info, err := GenerateBuildInfo("0.1.0", environment)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestMetadataBuildProvenance(t *testing.T) {
	for _, environment := range []map[string]string{
		{}, {"GITHUB_RUN_NUMBER": "123", "GITHUB_SHA": metadataTestCommit},
		{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": ReleaseRepository, "GITHUB_EVENT_NAME": "pull_request", "GITHUB_REF": "refs/pull/7/merge", "GITHUB_RUN_NUMBER": "123", "GITHUB_SHA": metadataTestCommit},
		{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "Other/project", "GITHUB_EVENT_NAME": "push", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_NUMBER": "123", "GITHUB_SHA": metadataTestCommit},
	} {
		info, err := GenerateBuildInfo("0.1.0", environment)
		if err != nil {
			t.Fatal(err)
		}
		if info.Build != nil || info.Commit != nil {
			t.Fatalf("local provenance invented: %+v", info)
		}
	}
	environment := map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": ReleaseRepository, "GITHUB_EVENT_NAME": "push", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_NUMBER": "123", "GITHUB_SHA": metadataTestCommit}
	info, err := GenerateBuildInfo("0.1.0", environment)
	if err != nil || info.Build == nil || *info.Build != 123 || *info.Commit != metadataTestCommit {
		t.Fatalf("published identity: %+v, %v", info, err)
	}
	delete(environment, "GITHUB_SHA")
	if _, err := GenerateBuildInfo("0.1.0", environment); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete published pipeline: %v", err)
	}
	environment["BUILD_NUMBER"] = "456"
	environment["SOURCE_COMMIT"] = metadataTestCommit
	info, err = GenerateBuildInfo("1.2.3-rc.1+package.4", environment)
	if err != nil || *info.Build != 456 {
		t.Fatalf("explicit provenance: %+v %v", info, err)
	}
}

func TestMetadataRejectsInvalidBuildPairs(t *testing.T) {
	for _, environment := range []map[string]string{
		{"BUILD_NUMBER": "1"}, {"SOURCE_COMMIT": metadataTestCommit},
		{"BUILD_NUMBER": "", "SOURCE_COMMIT": metadataTestCommit}, {"BUILD_NUMBER": "01", "SOURCE_COMMIT": metadataTestCommit},
		{"BUILD_NUMBER": "1000000000", "SOURCE_COMMIT": metadataTestCommit}, {"BUILD_NUMBER": "1", "SOURCE_COMMIT": "main"},
		{"BUILD_NUMBER": "1", "SOURCE_COMMIT": strings.Repeat("A", 40)},
	} {
		if _, err := GenerateBuildInfo("0.1.0", environment); err == nil {
			t.Errorf("accepted invalid pair: %v", environment)
		}
	}
}

func TestMetadataStrictFieldsAndTypes(t *testing.T) {
	info := metadataTestInfo(t, "123")
	raw, _ := json.Marshal(info)
	var valid map[string]any
	_ = json.Unmarshal(raw, &valid)
	for _, change := range []struct {
		field string
		value any
	}{
		{"schemaVersion", true}, {"schemaVersion", 2}, {"repository", "Other/project"}, {"build", true}, {"build", 0}, {"build", -1}, {"build", json.Number("1.0")},
		{"commit", nil}, {"commit", strings.Repeat("0", 39)}, {"commit", strings.Repeat("A", 40)},
		{"version", "01.2.3"}, {"version", "1.2"}, {"version", "1.2.3.4"}, {"version", "1.2.3-01"}, {"version", "1.2.3-rc..1"}, {"version", "1.2.3\n"}, {"version", `1.2.3";exit`}, {"url", "https://other.example/installer"},
	} {
		t.Run(change.field+"/"+strings.ReplaceAll(strings.TrimSpace(string(mustMetadataJSON(t, change.value))), "/", "_"), func(t *testing.T) {
			candidate := map[string]any{}
			for key, value := range valid {
				candidate[key] = value
			}
			candidate[change.field] = change.value
			if _, err := ValidateBuildInfo(mustMetadataJSON(t, candidate)); err == nil {
				t.Fatalf("accepted %v", candidate)
			}
		})
	}
	for _, raw := range []string{`null`, `[]`, `{}`, string(raw) + ` {}`, strings.TrimSuffix(string(raw), "}") + `,"build":123}`, strings.Replace(string(raw), `"schemaVersion":1`, `"schemaVersion":1.0`, 1)} {
		if _, err := ValidateBuildInfo([]byte(raw)); err == nil {
			t.Errorf("accepted malformed identity %s", raw)
		}
	}
	delete(valid, "commit")
	if _, err := ValidateBuildInfo(mustMetadataJSON(t, valid)); err == nil {
		t.Fatal("accepted missing commit")
	}
}

func mustMetadataJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestMetadataManifestPinsSourceAndArtifact(t *testing.T) {
	manifest := Manifest{BuildInfo: metadataTestInfo(t, "123"), Tag: "build-123-abcdef0", Asset: "Agent-Queue-0.1.0-universal.zip", SHA256: strings.Repeat("1", 64)}
	validated, err := ValidateManifest(mustMetadataJSON(t, manifest))
	if err != nil || validated.Tag != manifest.Tag {
		t.Fatalf("valid manifest: %+v %v", validated, err)
	}
	for _, change := range []struct {
		field string
		value any
	}{{"build", nil}, {"sha256", strings.Repeat("0", 63)}, {"tag", "build-124-abcdef0"}, {"asset", "../installer.zip"}, {"asset", "Agent-Job-Queue-Installer-0.1.0-universal.zip"}, {"asset", "Agent-Queue-0.1.1-universal.zip"}, {"unexpected", "value"}} {
		var value map[string]any
		_ = json.Unmarshal(mustMetadataJSON(t, manifest), &value)
		value[change.field] = change.value
		if _, err := ValidateManifest(mustMetadataJSON(t, value)); err == nil {
			t.Errorf("accepted manifest change %v", change)
		}
	}
}
