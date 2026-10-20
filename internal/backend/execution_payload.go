package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// Only private execution data uses byte strings. Public JSON, receipts and
// descriptions keep their existing schemas. JSON's []byte encoding supplies
// reversible base64 without interpreting Unix argv, environment or path bytes.
type executionBytes struct {
	Version int                    `json:"version"`
	Argv    [][]byte               `json:"argv"`
	Cwd     []byte                 `json:"cwd"`
	Env     []executionEnvironment `json:"env"`
}

type executionEnvironment struct {
	Name  []byte `json:"name"`
	Value []byte `json:"value"`
}

func executionPayloadFields(content Object) ([]string, string, map[string]string, error) {
	argv, err := stringsFrom(content["argv"])
	if err != nil || len(argv) == 0 {
		return nil, "", nil, errors.New("Invalid command argv")
	}
	cwd, ok := content["cwd"].(string)
	if !ok || strings.ContainsRune(cwd, 0) {
		return nil, "", nil, errors.New("Invalid job cwd/environment")
	}
	forwarded := map[string]string{}
	switch environment := content["env"].(type) {
	case map[string]string:
		for k, v := range environment {
			forwarded[k] = v
		}
	case Object:
		for k, raw := range environment {
			v, ok := raw.(string)
			if !ok {
				return nil, "", nil, errors.New("Invalid job cwd/environment")
			}
			forwarded[k] = v
		}
	default:
		return nil, "", nil, errors.New("Invalid job cwd/environment")
	}
	for k, v := range forwarded {
		if strings.ContainsAny(k, "\x00=") || strings.ContainsRune(v, 0) {
			return nil, "", nil, errors.New("Invalid job cwd/environment")
		}
	}
	return argv, cwd, forwarded, nil
}

func WriteJobPayload(path string, content Object) error {
	argv, cwd, environment, err := executionPayloadFields(content)
	if err != nil {
		return err
	}
	encoded := executionBytes{Version: 1, Argv: make([][]byte, len(argv)), Cwd: []byte(cwd), Env: []executionEnvironment{}}
	for i, arg := range argv {
		encoded.Argv[i] = []byte(arg)
	}
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		encoded.Env = append(encoded.Env, executionEnvironment{Name: []byte(key), Value: []byte(environment[key])})
	}
	wire := Object{}
	for key, value := range content {
		if key != "argv" && key != "cwd" && key != "env" && key != "execution_bytes" {
			wire[key] = value
		}
	}
	wire["execution_bytes"] = encoded
	return WritePrivate(path, wire)
}

func ReadJobPayload(path string) (Object, error) {
	content, err := ReadPrivate(path)
	if err != nil {
		return nil, err
	}
	if raw, present := content["execution_bytes"]; present {
		for _, key := range []string{"argv", "cwd", "env"} {
			if _, ambiguous := content[key]; ambiguous {
				return nil, errors.New("Ambiguous private execution payload")
			}
		}
		data, err := json.Marshal(raw)
		if err != nil {
			return nil, err
		}
		var encoded executionBytes
		shape := obj(raw)
		if shape == nil || len(shape) != 4 {
			return nil, errors.New("Invalid private execution byte payload")
		}
		if version, ok := integer(shape["version"]); !ok || version != 1 {
			return nil, errors.New("Invalid private execution byte version")
		}
		if _, ok := shape["cwd"].(string); !ok {
			return nil, errors.New("Invalid private execution byte directory")
		}
		if _, err := stringsFrom(shape["argv"]); err != nil {
			return nil, errors.New("Invalid private execution byte arguments")
		}
		entries, ok := shape["env"].([]any)
		if !ok {
			return nil, errors.New("Invalid private execution byte environment")
		}
		for _, raw := range entries {
			entry := obj(raw)
			if entry == nil || len(entry) != 2 {
				return nil, errors.New("Invalid private execution byte environment")
			}
			if _, ok := entry["name"].(string); !ok {
				return nil, errors.New("Invalid private execution byte environment")
			}
			if _, ok := entry["value"].(string); !ok {
				return nil, errors.New("Invalid private execution byte environment")
			}
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&encoded); err != nil || encoded.Version != 1 || encoded.Argv == nil || encoded.Cwd == nil || encoded.Env == nil {
			return nil, errors.New("Invalid private execution byte payload")
		}
		argv := make([]string, len(encoded.Argv))
		for i, arg := range encoded.Argv {
			if arg == nil {
				return nil, errors.New("Invalid private execution byte argument")
			}
			argv[i] = string(arg)
		}
		environment := Object{}
		for _, entry := range encoded.Env {
			if entry.Name == nil || entry.Value == nil {
				return nil, errors.New("Invalid private execution byte environment")
			}
			key := string(entry.Name)
			if _, duplicate := environment[key]; duplicate {
				return nil, errors.New("Duplicate private execution environment name")
			}
			environment[key] = string(entry.Value)
		}
		content["argv"] = argv
		content["cwd"] = string(encoded.Cwd)
		content["env"] = environment
		delete(content, "execution_bytes")
		if _, _, _, err = executionPayloadFields(content); err != nil {
			return nil, err
		}
	}
	// Legacy private JSON remains readable. Its consumers retain their existing
	// field validation, including metadata readers that need only argv and cwd.
	return content, nil
}
