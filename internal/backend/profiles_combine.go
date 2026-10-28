package backend

import (
	"fmt"
	"sort"
)

const maxLearningSelection = 32

func learningSelectionIDs(value any) ([]string, error) {
	switch value.(type) {
	case []any, []string:
	default:
		return nil, fmt.Errorf("Merge selections must be arrays of workload identities")
	}
	result := []string{}
	seen := map[string]bool{}
	for _, raw := range list(value) {
		id, ok := raw.(string)
		if !ok || profileID(id) != nil || seen[id] {
			return nil, fmt.Errorf("Choose each valid merge item only once")
		}
		seen[id] = true
		result = append(result, id)
	}
	return result, nil
}

func validateLearningSelection(profiles, families []string) error {
	if len(profiles)+len(families) < 1 || len(profiles)+len(families) > maxLearningSelection {
		return fmt.Errorf("Choose between 1 and 32 groups or workloads to merge")
	}
	for _, ids := range [][]string{profiles, families} {
		if _, err := learningSelectionIDs(ids); err != nil {
			return err
		}
	}
	return nil
}

type profileCombinePlan struct {
	families []string
	profiles map[string]Object
	groups   map[string]*profileGroup
}

func planProfileCombine(c profileSQL, target string, revision int, profileIDs, families []string) (*profileCombinePlan, error) {
	if err := profileID(target); err != nil {
		return nil, err
	}
	if err := validateLearningSelection(profileIDs, families); err != nil {
		return nil, err
	}
	if err := profileExpectedRevision(c, revision); err != nil {
		return nil, err
	}
	profiles, groups, err := profileDataset(c)
	if err != nil {
		return nil, err
	}
	if profiles[target] == nil {
		return nil, fmt.Errorf("Destination group no longer exists; refresh learning")
	}
	plan := &profileCombinePlan{profiles: map[string]Object{target: profiles[target]}, groups: groups}
	selected := map[string]bool{}
	owners := map[string]string{}
	for id, group := range groups {
		for _, member := range group.members {
			owners[text(member["family"])] = id
		}
	}
	for _, id := range profileIDs {
		if id == target {
			return nil, fmt.Errorf("The destination cannot also be a source group")
		}
		if profiles[id] == nil {
			return nil, fmt.Errorf("A selected group no longer exists; refresh and review the selection")
		}
		plan.profiles[id] = profiles[id]
		for _, member := range groups[id].members {
			selected[text(member["family"])] = true
		}
	}
	for _, family := range families {
		owner, ok := owners[family]
		if !ok {
			return nil, fmt.Errorf("A selected workload no longer exists; refresh and review the selection")
		}
		if owner == target {
			return nil, fmt.Errorf("A selected workload already belongs to the destination group")
		}
		plan.profiles[owner] = profiles[owner]
		selected[family] = true
	}
	for family := range selected {
		plan.families = append(plan.families, family)
	}
	sort.Strings(plan.families)
	return plan, nil
}

func (s *ProfileStore) CheckCombine(target string, revision int, profiles, families []string) error {
	return s.connection(false, func(c profileSQL) error {
		_, err := planProfileCombine(c, target, revision, profiles, families)
		return err
	})
}

// Combine changes membership once, in one transaction, without rewriting history.
func (s *ProfileStore) Combine(target string, revision int, profiles, families []string) error {
	return s.connection(true, func(c profileSQL) error {
		plan, err := planProfileCombine(c, target, revision, profiles, families)
		if err != nil {
			return err
		}
		ids := []string{}
		for id := range plan.profiles {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		memory, cpu, ignore := 0.0, 0.0, 1
		for _, id := range ids {
			if _, _, err = profileEditable(c, id); err != nil {
				return err
			}
			profile := plan.profiles[id]
			memory = max(memory, metric(profile["memory_floor"]))
			cpu = max(cpu, metric(profile["cpu_floor"]))
			if metric(profile["ignore_legacy"]) == 0 {
				ignore = 0
			}
		}
		for _, family := range plan.families {
			if err = profileExec(c, "UPDATE learning_members SET profile=? WHERE family=?", target, family); err != nil {
				return err
			}
		}
		for _, id := range ids {
			if id == target {
				continue
			}
			remaining, err := profileRow(c, "SELECT 1 FROM learning_members WHERE profile=? LIMIT 1", id)
			if err != nil {
				return err
			}
			if remaining == nil {
				err = profileExec(c, "DELETE FROM learning_profiles WHERE id=?", id)
			} else {
				err = profileExec(c, "UPDATE learning_profiles SET pinned=1,updated=? WHERE id=?", resourceNow(), id)
			}
			if err != nil {
				return err
			}
		}
		var memoryValue, cpuValue any
		if memory > 0 {
			memoryValue = int64(memory)
		}
		if cpu > 0 {
			cpuValue = cpu
		}
		if err = profileExec(c, "UPDATE learning_profiles SET label=?,memory_floor=?,cpu_floor=?,ignore_legacy=?,pinned=1,updated=? WHERE id=?", profileGroupLabel(plan.profiles[target], plan.groups[target].members), memoryValue, cpuValue, ignore, resourceNow(), target); err != nil {
			return err
		}
		return profileChanged(c)
	})
}
