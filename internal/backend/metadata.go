package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const ReleaseRepository = "Dunky13/agent-job-queue"
const MaxReleaseBuild = 999999999

var metadataVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)\.)*(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
var metadataCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)
var metadataChecksum = regexp.MustCompile(`^[0-9a-f]{64}$`)
var metadataBuildNumber = regexp.MustCompile(`^[1-9][0-9]{0,8}$`)

type BuildInfo struct {
	SchemaVersion int     `json:"schemaVersion"`
	Repository    string  `json:"repository"`
	Version       string  `json:"version"`
	Commit        *string `json:"commit"`
	Build         *int    `json:"build"`
}

type Manifest struct {
	BuildInfo
	Tag    string `json:"tag"`
	Asset  string `json:"asset"`
	SHA256 string `json:"sha256"`
}

func metadataFields(data []byte, fields ...string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("Unexpected build identity fields")
	}
	values := make(map[string]json.RawMessage)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, errors.New("Invalid metadata key")
		}
		if _, exists := values[name]; exists {
			return nil, errors.New("Duplicate metadata field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		values[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("Invalid trailing metadata")
	}
	if len(values) != len(fields) {
		return nil, errors.New("Unexpected build identity fields")
	}
	for _, field := range fields {
		if _, ok := values[field]; !ok {
			return nil, errors.New("Unexpected build identity fields")
		}
	}
	return values, nil
}

func metadataBuild(values map[string]json.RawMessage) (BuildInfo, error) {
	var info BuildInfo
	if string(values["schemaVersion"]) != "1" || json.Unmarshal(values["repository"], &info.Repository) != nil || info.Repository != ReleaseRepository {
		return info, errors.New("Unsupported build identity source")
	}
	info.SchemaVersion = 1
	if json.Unmarshal(values["version"], &info.Version) != nil || !metadataVersion.MatchString(info.Version) {
		return info, errors.New("Invalid version")
	}
	if err := json.Unmarshal(values["commit"], &info.Commit); err != nil || (info.Commit != nil && !metadataCommit.MatchString(*info.Commit)) {
		return info, errors.New("Invalid commit")
	}
	if err := json.Unmarshal(values["build"], &info.Build); err != nil || (info.Build != nil && (*info.Build < 1 || *info.Build > MaxReleaseBuild)) {
		return info, errors.New("Invalid build number")
	}
	if info.Build != nil && info.Commit == nil {
		return info, errors.New("Tracked builds require a commit")
	}
	return info, nil
}

func ValidateBuildInfo(data []byte) (BuildInfo, error) {
	values, err := metadataFields(data, "schemaVersion", "repository", "version", "commit", "build")
	if err != nil {
		return BuildInfo{}, err
	}
	return metadataBuild(values)
}

func ValidateManifest(data []byte) (Manifest, error) {
	values, err := metadataFields(data, "schemaVersion", "repository", "version", "commit", "build", "tag", "asset", "sha256")
	if err != nil {
		return Manifest{}, err
	}
	info, err := metadataBuild(values)
	if err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{BuildInfo: info}
	if info.Build == nil {
		return manifest, errors.New("Release manifests require a build number")
	}
	if json.Unmarshal(values["sha256"], &manifest.SHA256) != nil || !metadataChecksum.MatchString(manifest.SHA256) {
		return manifest, errors.New("Invalid sha256")
	}
	if json.Unmarshal(values["tag"], &manifest.Tag) != nil || manifest.Tag != fmt.Sprintf("build-%d-%.7s", *info.Build, *info.Commit) {
		return manifest, errors.New("Release tag does not match the build and commit")
	}
	if json.Unmarshal(values["asset"], &manifest.Asset) != nil || manifest.Asset != fmt.Sprintf("Agent-Job-Queue-Installer-%s-universal.zip", info.Version) {
		return manifest, errors.New("Unexpected installer asset")
	}
	return manifest, nil
}

func (info BuildInfo) Equal(other BuildInfo) bool {
	if info.SchemaVersion != other.SchemaVersion || info.Repository != other.Repository || info.Version != other.Version || (info.Build == nil) != (other.Build == nil) || (info.Commit == nil) != (other.Commit == nil) {
		return false
	}
	return (info.Build == nil || *info.Build == *other.Build) && (info.Commit == nil || *info.Commit == *other.Commit)
}

func GenerateBuildInfo(version string, environment map[string]string) (BuildInfo, error) {
	if environment == nil {
		environment = make(map[string]string)
		for _, entry := range os.Environ() {
			key, value, _ := strings.Cut(entry, "=")
			environment[key] = value
		}
	}
	build, hasBuild := environment["BUILD_NUMBER"]
	commit, hasCommit := environment["SOURCE_COMMIT"]
	if hasBuild != hasCommit {
		return BuildInfo{}, errors.New("BUILD_NUMBER and SOURCE_COMMIT must be supplied together")
	}
	if !hasBuild && environment["GITHUB_ACTIONS"] == "true" && environment["GITHUB_REPOSITORY"] == ReleaseRepository && environment["GITHUB_EVENT_NAME"] == "push" && environment["GITHUB_REF"] == "refs/heads/main" {
		build, hasBuild = environment["GITHUB_RUN_NUMBER"]
		commit, hasCommit = environment["GITHUB_SHA"]
		if !hasBuild || !hasCommit {
			return BuildInfo{}, errors.New("Published CI build identity is incomplete")
		}
	}
	info := BuildInfo{SchemaVersion: 1, Repository: ReleaseRepository, Version: version}
	if hasBuild {
		if !metadataBuildNumber.MatchString(build) {
			return info, errors.New("Invalid build number")
		}
		number, _ := strconv.Atoi(build)
		info.Build = &number
		info.Commit = &commit
	}
	data, err := json.Marshal(info)
	if err != nil {
		return info, err
	}
	return ValidateBuildInfo(data)
}

func MetadataMain(args []string) int {
	var version, output, field string
	for len(args) > 0 {
		if len(args) < 2 || (args[0] != "--version" && args[0] != "--output" && args[0] != "--field") {
			fmt.Fprintln(os.Stderr, "queue-build-info: --version and --output are required")
			return 2
		}
		if args[0] == "--version" {
			version = args[1]
		} else if args[0] == "--output" {
			output = args[1]
		} else {
			field = args[1]
		}
		args = args[2:]
	}
	if version == "" || (output == "" && field == "") || (field != "" && field != "build" && field != "commit" && field != "version") {
		fmt.Fprintln(os.Stderr, "queue-build-info: --version and --output are required")
		return 2
	}
	info, err := GenerateBuildInfo(version, nil)
	if err == nil && field != "" {
		switch field {
		case "build":
			if info.Build != nil {
				fmt.Println(*info.Build)
			} else {
				fmt.Println(0)
			}
		case "commit":
			if info.Commit != nil {
				fmt.Println(*info.Commit)
			}
		case "version":
			fmt.Println(info.Version)
		}
		if output == "" {
			return 0
		}
	}
	if err == nil {
		var data []byte
		data, err = json.MarshalIndent(info, "", "  ")
		if err == nil {
			err = os.WriteFile(output, append(data, '\n'), 0600)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "queue-build-info: %v\n", err)
		return 1
	}
	return 0
}
