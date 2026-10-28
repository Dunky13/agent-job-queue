package preview

import (
	"errors"
	"slices"
)

func (q *fakeQueue) combineProfiles(targetID string, value map[string]any, arguments []string) (map[string]any, error) {
	profiles := clone(q.profiles)
	destination, exists := profiles[targetID]
	if !exists {
		return nil, errors.New("Destination group no longer exists")
	}
	owners := map[string]string{}
	for id, detail := range profiles {
		all, err := members(detail["members"])
		if err != nil {
			return nil, err
		}
		for _, member := range all {
			family, _ := member["family"].(string)
			owners[family] = id
		}
	}
	groupIDs, err := fixtureSelectionIDs(value["profileIds"])
	if err != nil {
		return nil, err
	}
	familyIDs, err := fixtureSelectionIDs(value["families"])
	if err != nil {
		return nil, err
	}
	if len(groupIDs)+len(familyIDs) < 1 || len(groupIDs)+len(familyIDs) > 32 {
		return nil, errors.New("Invalid fixture merge selection size")
	}
	selected, involved := map[string]bool{}, map[string]bool{}
	for _, raw := range groupIDs {
		source, exists := profiles[raw]
		if !exists || raw == targetID {
			return nil, errors.New("Invalid selected source group")
		}
		involved[raw] = true
		all, err := members(source["members"])
		if err != nil {
			return nil, err
		}
		for _, member := range all {
			family, _ := member["family"].(string)
			selected[family] = true
		}
	}
	for _, family := range familyIDs {
		owner, exists := owners[family]
		if !exists || owner == targetID {
			return nil, errors.New("Invalid selected workload")
		}
		selected[family] = true
		involved[owner] = true
	}
	if len(selected) == 0 {
		return nil, errors.New("No merge sources were selected")
	}
	targetMembers, err := members(destination["members"])
	if err != nil {
		return nil, err
	}
	targetProfile, _ := object(destination["profile"])
	for id := range involved {
		source := profiles[id]
		all, err := members(source["members"])
		if err != nil {
			return nil, err
		}
		for _, member := range all {
			family, _ := member["family"].(string)
			if selected[family] {
				targetMembers = append(targetMembers, member)
			}
		}
		source["members"] = slices.DeleteFunc(all, func(member map[string]any) bool { family, _ := member["family"].(string); return selected[family] })
		for _, key := range []string{"variants", "runs"} {
			sourceRows, err := members(source[key])
			if err != nil {
				return nil, err
			}
			targetRows, err := members(destination[key])
			if err != nil {
				return nil, err
			}
			for _, row := range sourceRows {
				family, _ := row["family"].(string)
				if selected[family] {
					targetRows = append(targetRows, row)
				}
			}
			destination[key] = targetRows
		}
		sourceProfile, _ := object(source["profile"])
		for _, key := range []string{"memoryFloorBytes", "cpuFloorCores"} {
			floor, _ := sourceProfile[key].(float64)
			current, _ := targetProfile[key].(float64)
			if floor > current {
				targetProfile[key] = floor
			}
		}
		remaining, _ := members(source["members"])
		sourceProfile["memberCount"] = len(remaining)
		if len(remaining) == 0 {
			delete(profiles, id)
		} else if err = retainProfileMembers(source); err != nil {
			return nil, err
		}
	}
	destination["members"] = targetMembers
	targetProfile["memberCount"] = len(targetMembers)
	if err = retainProfileMembers(destination); err != nil {
		return nil, err
	}
	result, err := inspectProfile(destination, []string{"--json"})
	if err != nil {
		return nil, err
	}
	q.profiles = profiles
	q.profileRevision++
	q.events = append(q.events, append([]string(nil), arguments...))
	result["revision"] = q.profileRevision
	return result, nil
}
func fixtureSelectionIDs(value any) ([]string, error) {
	result := []string{}
	values, ok := value.([]any)
	if !ok {
		return nil, errors.New("Fixture selections must be arrays")
	}
	seen := map[string]bool{}
	for _, raw := range values {
		id, ok := raw.(string)
		if !ok || seen[id] {
			return nil, errors.New("Invalid or duplicate fixture selection")
		}
		seen[id] = true
		result = append(result, id)
	}
	return result, nil
}
