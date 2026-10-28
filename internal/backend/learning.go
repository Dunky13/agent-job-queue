package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxLearningInput = 16384

func ValidateLearningRequest(operation string, value Object) (Object, error) {
	fields := map[string][]string{"edit": {"revision", "label", "memoryFloorBytes", "cpuFloorCores"}, "merge": {"revision", "targetId"}, "combine": {"revision", "profileIds", "families"}, "split": {"revision", "family"}, "reset": {"revision", "confirm"}}
	expected, ok := fields[operation]
	if !ok || value == nil || len(value) != len(expected) {
		return nil, fmt.Errorf("Invalid workload correction fields")
	}
	for _, field := range expected {
		if _, ok := value[field]; !ok {
			return nil, fmt.Errorf("Invalid workload correction fields")
		}
	}
	revision, ok := schedulingInteger(value["revision"])
	if !ok || revision < 0 || revision > math.MaxInt64-1 {
		return nil, fmt.Errorf("Invalid expected learning revision")
	}
	result := Object{}
	for k, v := range value {
		result[k] = v
	}
	if operation == "edit" {
		label, ok := value["label"].(string)
		label = strings.TrimSpace(label)
		if !ok || len([]rune(label)) < 1 || len([]rune(label)) > 120 || !profilePrintable(label) {
			return nil, fmt.Errorf("A profile name must contain 1 to 120 printable characters")
		}
		result["label"] = label
		if value["memoryFloorBytes"] != nil {
			n, ok := schedulingInteger(value["memoryFloorBytes"])
			if !ok || n <= 0 || n > 1<<50 {
				return nil, fmt.Errorf("Minimum memory must be a positive integer no greater than 2^50 bytes")
			}
		}
		if value["cpuFloorCores"] != nil {
			n, ok := number(value["cpuFloorCores"])
			if !ok || n <= 0 || n > 1024 {
				return nil, fmt.Errorf("Minimum CPU must be greater than zero and no greater than 1024 cores")
			}
		}
	} else if operation == "merge" {
		if e := profileID(text(value["targetId"])); e != nil {
			return nil, e
		}
	} else if operation == "combine" {
		profiles, err := learningSelectionIDs(value["profileIds"])
		if err != nil {
			return nil, err
		}
		families, err := learningSelectionIDs(value["families"])
		if err != nil {
			return nil, err
		}
		if err = validateLearningSelection(profiles, families); err != nil {
			return nil, err
		}
		result["profileIds"], result["families"] = profiles, families
	} else if operation == "split" {
		if e := profileID(text(value["family"])); e != nil {
			return nil, e
		}
	} else if !boolean(value["confirm"]) {
		return nil, fmt.Errorf("Reset requires explicit confirmation")
	}
	return result, nil
}
func learningController(g *Gate) (string, any, string) {
	state, e := SchedulingView(g)
	if e != nil {
		return "", nil, "Controller status is unavailable; saved learning remains visible"
	}
	var applied any
	if revision, ok := schedulingInteger(state["learningRevision"]); ok && revision >= 0 {
		applied = revision
	}
	machine := text(obj(state["host"])["machine"])
	reason := text(state["reason"])
	if boolean(state["available"]) {
		reason = "Current controller predictions include these corrections"
	} else if reason == "" {
		reason = "Waiting for fresh controller predictions"
	}
	return machine, applied, reason
}
func LearningInspect(g *Gate, identity string, offset, limit int, query string, detailOptions ...ProfileDetailOptions) (Object, error) {
	machine, applied, status := learningController(g)
	store, e := NewProfileStore(g.State, true)
	if e != nil {
		return nil, e
	}
	defer store.Close()
	var result Object
	if identity != "" {
		if e = profileID(identity); e != nil {
			return nil, e
		}
		options := ProfileDetailOptions{MemberLimit: 256}
		if len(detailOptions) > 0 {
			options = detailOptions[0]
		}
		result, e = store.Inspect(identity, machine, options)
	} else {
		result, e = store.Catalog(offset, limit, query)
	}
	if e != nil {
		return nil, e
	}
	revision, _ := integer(result["revision"])
	appliedRevision, ok := integer(applied)
	if !ok || appliedRevision != revision {
		status = "Saved learning is awaiting fresh controller predictions. " + status
	}
	result["controllerAppliedRevision"] = applied
	result["controllerStatus"] = status
	return result, nil
}
func LearningCorrect(g *Gate, operation, identity string, value Object) (Object, error) {
	if e := profileID(identity); e != nil {
		return nil, e
	}
	request, e := ValidateLearningRequest(operation, value)
	if e != nil {
		return nil, e
	}
	if e = g.Prepare(); e != nil {
		return nil, e
	}
	unlockScheduling, e := LockPrivate(g, "scheduling.lock")
	if e != nil {
		return nil, e
	}
	defer unlockScheduling()
	unlockLease, e := LockPrivate(g, "")
	if e != nil {
		return nil, e
	}
	defer unlockLease()
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		return nil, e
	}
	defer store.Close()
	revision, _ := integer(request["revision"])
	current, e := store.Revision()
	if e != nil {
		return nil, e
	}
	if current != revision {
		return nil, fmt.Errorf("Learning changed since you opened it; refresh before saving")
	}
	detail, e := store.Detail(identity, "")
	if e != nil {
		return nil, e
	}
	if operation == "merge" {
		if identity == text(request["targetId"]) {
			return nil, fmt.Errorf("Choose a different profile to share learning with")
		}
		if _, e = store.Detail(text(request["targetId"]), ""); e != nil {
			return nil, e
		}
	}
	if operation == "split" {
		memberCount, _ := integer(obj(detail["profile"])["memberCount"])
		found := false
		for _, raw := range list(detail["members"]) {
			found = found || text(obj(raw)["family"]) == text(request["family"])
		}
		if memberCount < 2 || (!boolean(obj(detail["truncated"])["members"]) && !found) {
			return nil, fmt.Errorf("Choose a member family from a shared profile to separate")
		}
	}
	if operation == "combine" {
		if e = store.CheckCombine(identity, revision, request["profileIds"].([]string), request["families"].([]string)); e != nil {
			return nil, e
		}
	}
	if e = invalidateLearningAdmissions(g); e != nil {
		return nil, e
	}
	switch operation {
	case "edit":
		e = store.Edit(identity, revision, text(request["label"]), request["memoryFloorBytes"], request["cpuFloorCores"])
	case "merge":
		e = store.Merge(identity, text(request["targetId"]), revision)
		identity = text(request["targetId"])
	case "combine":
		e = store.Combine(identity, revision, request["profileIds"].([]string), request["families"].([]string))
	case "split":
		identity, e = store.Split(identity, text(request["family"]), revision)
	case "reset":
		e = store.Reset(identity, revision)
	}
	if e != nil {
		return nil, e
	}
	return LearningInspect(g, identity, 0, 100, "")
}

// Callers hold scheduling.lock and lease.lock, and have checked the revision.
func invalidateLearningAdmissions(g *Gate) error {
	previous, e := SchedulingSettings(g)
	if e != nil {
		return e
	}
	if previous["mode"] == "adaptive" {
		state, e := SchedulingView(g)
		if e != nil {
			return e
		}
		if _, ok := schedulingInteger(state["learningRevision"]); !ok {
			return fmt.Errorf("Wait for the updated resource controller before correcting adaptive learning, or select Fixed scheduling")
		}
	}
	admissions, e := SchedulingLedger(g, previous)
	if e != nil {
		return e
	}
	configuration := Object{"mode": previous["mode"], "generation": token()}
	admissions["generation"] = configuration["generation"]
	if e = retainActiveGrants(g, admissions); e != nil {
		return e
	}
	if e = WritePrivate(filepath.Join(g.State, "resource-admissions.json"), admissions); e != nil {
		return e
	}
	return WritePrivate(filepath.Join(g.State, "scheduling.json"), configuration)
}
func learningJSONValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, fmt.Errorf("JSON nesting exceeds its safe limit")
	}
	token, e := decoder.Token()
	if e != nil {
		return nil, e
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}
	switch delimiter {
	case '{':
		result := Object{}
		for decoder.More() {
			raw, e := decoder.Token()
			if e != nil {
				return nil, e
			}
			key, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("Invalid object field")
			}
			if _, ok = result[key]; ok {
				return nil, fmt.Errorf("Duplicate object field")
			}
			result[key], e = learningJSONValue(decoder, depth+1)
			if e != nil {
				return nil, e
			}
		}
		_, e = decoder.Token()
		return result, e
	case '[':
		result := []any{}
		for decoder.More() {
			v, e := learningJSONValue(decoder, depth+1)
			if e != nil {
				return nil, e
			}
			result = append(result, v)
		}
		_, e = decoder.Token()
		return result, e
	}
	return nil, fmt.Errorf("Invalid JSON value")
}
func ReadLearningInput(input io.Reader) (Object, error) {
	if file, ok := input.(*os.File); ok {
		info, e := file.Stat()
		if e != nil {
			return nil, e
		}
		if info.Mode()&os.ModeCharDevice != 0 {
			return nil, fmt.Errorf("Supply the correction JSON through private standard input")
		}
	}
	data, e := io.ReadAll(io.LimitReader(input, maxLearningInput+1))
	if e != nil {
		return nil, e
	}
	if len(data) > maxLearningInput {
		return nil, fmt.Errorf("Workload correction exceeds its size limit")
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("Workload correction must be a JSON object with unique fields")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, e := learningJSONValue(decoder, 0)
	if e != nil {
		return nil, fmt.Errorf("Workload correction must be a JSON object with unique fields")
	}
	if _, e = decoder.Token(); e != io.EOF {
		return nil, fmt.Errorf("Workload correction must be a JSON object with unique fields")
	}
	result := obj(value)
	if result == nil {
		return nil, fmt.Errorf("Workload correction must be a JSON object with unique fields")
	}
	return result, nil
}
func LearningMain(args []string) int {
	gate, e := NewGate()
	if e != nil {
		fmt.Fprintln(os.Stderr, "agent-heavy:", e)
		return 125
	}
	return LearningCLI(gate, args, os.Stdin, os.Stdout, os.Stderr)
}
func LearningCLI(gate *Gate, args []string, input io.Reader, output, errors io.Writer) int {
	jsonOutput := false
	for _, arg := range args {
		jsonOutput = jsonOutput || arg == "--json"
	}
	fail := func(e error) int {
		if jsonOutput {
			_ = json.NewEncoder(output).Encode(Object{"error": e.Error()})
		} else {
			fmt.Fprintln(errors, "agent-heavy:", e)
		}
		return 125
	}
	if len(args) == 0 {
		return fail(fmt.Errorf("A workload profile operation is required"))
	}
	operation := args[0]
	if !containsString([]string{"list", "show", "edit", "merge", "combine", "split", "reset", "regroup"}, operation) {
		return fail(fmt.Errorf("Invalid workload profile operation"))
	}
	identity, query := "", ""
	offset, limit := 0, 100
	detailOptions := ProfileDetailOptions{MemberLimit: 256}
	seenDetailOptions := map[string]bool{}
	apply := false
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--json" {
			continue
		}
		if operation == "show" {
			key, value, attached := strings.Cut(arg, "=")
			if containsString([]string{"--family", "--members-query", "--members-offset", "--members-limit"}, key) {
				if seenDetailOptions[key] {
					return fail(fmt.Errorf("Duplicate group inspection option"))
				}
				seenDetailOptions[key] = true
				if !attached {
					i++
					if i >= len(args) {
						return fail(fmt.Errorf("Missing group inspection option value"))
					}
					value = args[i]
				}
				switch key {
				case "--family":
					if e := profileID(value); e != nil {
						return fail(e)
					}
					detailOptions.Family = value
				case "--members-query":
					detailOptions.MemberQuery = value
				case "--members-offset", "--members-limit":
					n, e := strconv.Atoi(value)
					if e != nil {
						return fail(fmt.Errorf("Invalid group member page"))
					}
					if key == "--members-offset" {
						detailOptions.MemberOffset = n
					} else {
						detailOptions.MemberLimit = n
					}
				}
				continue
			}
		}
		if operation == "regroup" {
			if arg == "--apply" && !apply {
				apply = true
				continue
			}
			return fail(fmt.Errorf("Invalid workload regroup argument"))
		}
		if operation == "list" {
			key, value, attached := strings.Cut(arg, "=")
			if containsString([]string{"--offset", "--limit", "--query"}, key) {
				if !attached {
					i++
					if i >= len(args) {
						return fail(fmt.Errorf("Missing workload profile option value"))
					}
					value = args[i]
				}
				switch key {
				case "--offset", "--limit":
					n, e := strconv.Atoi(value)
					if e != nil {
						return fail(fmt.Errorf("Invalid workload profile page"))
					}
					if key == "--offset" {
						offset = n
					} else {
						limit = n
					}
				case "--query":
					query = value
				}
				continue
			}
		}
		if operation != "list" && identity == "" && !strings.HasPrefix(arg, "-") {
			identity = arg
			continue
		}
		return fail(fmt.Errorf("Invalid workload profile argument"))
	}
	if operation != "list" && operation != "regroup" && identity == "" {
		return fail(fmt.Errorf("A workload profile identity is required"))
	}
	var result Object
	var e error
	if operation == "regroup" {
		var request Object
		if apply {
			request, e = ReadLearningInput(input)
		}
		if e == nil {
			result, e = LearningRegroup(gate, apply, request)
		}
	} else if operation == "list" || operation == "show" {
		result, e = LearningInspect(gate, identity, offset, limit, query, detailOptions)
	} else {
		var request Object
		request, e = ReadLearningInput(input)
		if e == nil {
			result, e = LearningCorrect(gate, operation, identity, request)
		}
	}
	if e != nil {
		return fail(e)
	}
	encoder := json.NewEncoder(output)
	if !jsonOutput {
		encoder.SetIndent("", "  ")
	}
	if e = encoder.Encode(result); e != nil {
		return fail(e)
	}
	return 0
}
