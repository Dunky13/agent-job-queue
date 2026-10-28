package backend

import (
	"fmt"
	"golang.org/x/text/cases"
	"sort"
	"strings"
)

type profileGroup struct{ members, runs, envelopes []Object }

func profileDataset(c profileSQL) (map[string]Object, map[string]*profileGroup, error) {
	profiles, members := map[string]Object{}, map[string]Object{}
	has, e := hasProfileLearning(c)
	if e != nil {
		return nil, nil, e
	}
	if has {
		rows, e := profileRows(c, "SELECT * FROM learning_profiles")
		if e != nil {
			return nil, nil, e
		}
		for _, row := range rows {
			profiles[text(row["id"])] = row
		}
		rows, e = profileRows(c, "SELECT * FROM learning_members")
		if e != nil {
			return nil, nil, e
		}
		for _, row := range rows {
			members[text(row["family"])] = row
		}
	}
	runs, e := profileRows(c, "SELECT * FROM runs")
	if e != nil {
		return nil, nil, e
	}
	envelopes, e := profileRows(c, "SELECT * FROM envelopes")
	if e != nil {
		return nil, nil, e
	}
	if has {
		retained, e := profileRows(c, "SELECT * FROM learning_retained")
		if e != nil {
			return nil, nil, e
		}
		envelopes = append(envelopes, retained...)
	}
	for _, row := range append(append([]Object{}, runs...), envelopes...) {
		family := text(row["family"])
		if members[family] == nil {
			if profiles[family] != nil {
				return nil, nil, fmt.Errorf("Workload grouping is inconsistent")
			}
			profiles[family] = Object{"id": family, "label": nil, "memory_floor": nil, "cpu_floor": nil, "pinned": 0, "ignore_legacy": 0, "updated": row["sampled"]}
			members[family] = Object{"family": family, "profile": family, "title": nil, "project": nil, "executable": nil}
		}
	}
	groups := map[string]*profileGroup{}
	for id := range profiles {
		groups[id] = &profileGroup{members: []Object{}, runs: []Object{}, envelopes: []Object{}}
	}
	for _, member := range members {
		group := groups[text(member["profile"])]
		if group == nil {
			return nil, nil, fmt.Errorf("Workload grouping is inconsistent")
		}
		group.members = append(group.members, member)
	}
	for _, row := range runs {
		group := groups[text(members[text(row["family"])]["profile"])]
		group.runs = append(group.runs, row)
	}
	for _, row := range envelopes {
		group := groups[text(members[text(row["family"])]["profile"])]
		group.envelopes = append(group.envelopes, row)
	}
	return profiles, groups, nil
}
func profileSummary(profile Object, group *profileGroup) (Object, error) {
	if len(group.members) == 0 {
		return nil, fmt.Errorf("Workload profile has no members")
	}
	variants := map[string]bool{}
	successes, incomplete := 0, 0
	peakMemory, peakCPU := 0.0, 0.0
	var last any
	for _, row := range append(append([]Object{}, group.runs...), group.envelopes...) {
		variants[text(row["family"])+text(row["variant"])] = true
		peakMemory = max(peakMemory, metric(row["memory"]))
		peakCPU = max(peakCPU, metric(row["cpu"]))
		if last == nil || metric(row["sampled"]) > metric(last) {
			last = row["sampled"]
		}
	}
	for _, row := range group.runs {
		if metric(row["success"]) == 1 {
			successes++
		} else {
			incomplete++
		}
	}
	return Object{"id": profile["id"], "label": profileGroupLabel(profile, group.members), "memberCount": len(group.members), "variantCount": len(variants), "successCount": successes, "incompleteCount": incomplete, "peakMemoryBytes": int64(peakMemory), "peakCpuCores": peakCPU, "lastRunAt": last, "memoryFloorBytes": profile["memory_floor"], "cpuFloorCores": profile["cpu_floor"]}, nil
}
func (s *ProfileStore) Catalog(offset, limit int, query string) (Object, error) {
	if offset < 0 || limit < 1 || limit > 100 {
		return nil, fmt.Errorf("Invalid workload profile page")
	}
	if len([]rune(query)) > 120 || !profilePrintable(query) {
		return nil, fmt.Errorf("Invalid workload profile search")
	}
	result := Object{"revision": 0, "profiles": []Object{}, "total": 0, "offset": offset, "limit": limit}
	if s.empty {
		return result, nil
	}
	e := s.connection(false, func(c profileSQL) error {
		revision, e := profileRevision(c)
		if e != nil {
			return e
		}
		profiles, groups, e := profileDataset(c)
		if e != nil {
			return e
		}
		entries := []Object{}
		needle := strings.TrimSpace(cases.Fold().String(query))
		for id, profile := range profiles {
			summary, e := profileSummary(profile, groups[id])
			if e != nil {
				return e
			}
			if needle == "" || strings.Contains(cases.Fold().String(text(summary["label"])), needle) || strings.Contains(id, needle) {
				entries = append(entries, summary)
			}
		}
		sort.Slice(entries, func(i, j int) bool {
			a, b := cases.Fold().String(text(entries[i]["label"])), cases.Fold().String(text(entries[j]["label"]))
			if a == b {
				return text(entries[i]["id"]) < text(entries[j]["id"])
			}
			return a < b
		})
		result = Object{"revision": revision, "profiles": entries[min(offset, len(entries)) : min(offset, len(entries))+min(limit, len(entries)-min(offset, len(entries)))], "total": len(entries), "offset": offset, "limit": limit}
		return nil
	})
	return result, e
}

type ProfileDetailOptions struct {
	Family       string
	MemberOffset int
	MemberLimit  int
	MemberQuery  string
}

func (s *ProfileStore) Detail(id, machine string) (Object, error) {
	return s.Inspect(id, machine, ProfileDetailOptions{MemberLimit: 256})
}

func (s *ProfileStore) Inspect(id, machine string, options ProfileDetailOptions) (Object, error) {
	if e := profileID(id); e != nil {
		return nil, e
	}
	if options.Family != "" {
		if e := profileID(options.Family); e != nil {
			return nil, e
		}
		if options.MemberOffset != 0 || options.MemberQuery != "" {
			return nil, fmt.Errorf("Choose a workload or search group members separately")
		}
	}
	if options.MemberOffset < 0 || options.MemberLimit < 1 || options.MemberLimit > 256 || len([]rune(options.MemberQuery)) > 120 || !profilePrintable(options.MemberQuery) {
		return nil, fmt.Errorf("Invalid group member page or search")
	}
	if s.empty {
		return nil, fmt.Errorf("Workload profile was not found")
	}
	var result Object
	e := s.connection(false, func(c profileSQL) error {
		profiles, groups, e := profileDataset(c)
		if e != nil {
			return e
		}
		profile, group := profiles[id], groups[id]
		if profile == nil {
			return fmt.Errorf("Workload profile was not found; refresh the list")
		}
		summary, e := profileSummary(profile, group)
		if e != nil {
			return e
		}
		originalGroup := group
		if options.Family != "" {
			filtered := &profileGroup{}
			for _, member := range group.members {
				if text(member["family"]) == options.Family {
					filtered.members = append(filtered.members, member)
				}
			}
			if len(filtered.members) != 1 {
				return fmt.Errorf("That workload does not belong to this group; refresh learning")
			}
			for _, row := range group.runs {
				if text(row["family"]) == options.Family {
					filtered.runs = append(filtered.runs, row)
				}
			}
			for _, row := range group.envelopes {
				if text(row["family"]) == options.Family {
					filtered.envelopes = append(filtered.envelopes, row)
				}
			}
			group = filtered
		}
		sort.Slice(group.members, func(i, j int) bool { return text(group.members[i]["family"]) < text(group.members[j]["family"]) })
		variants := map[string]Object{}
		for _, row := range append(append([]Object{}, group.envelopes...), group.runs...) {
			key := text(row["family"]) + text(row["variant"])
			item := variants[key]
			if item == nil {
				item = Object{"id": row["variant"], "family": row["family"], "successCount": 0, "incompleteCount": 0, "lastRunAt": 0.0, "peakMemoryBytes": int64(0), "peakCpuCores": 0.0}
				variants[key] = item
			}
			item["lastRunAt"] = max(metric(item["lastRunAt"]), metric(row["sampled"]))
			item["peakMemoryBytes"] = int64(max(metric(item["peakMemoryBytes"]), metric(row["memory"])))
			item["peakCpuCores"] = max(metric(item["peakCpuCores"]), metric(row["cpu"]))
			if success, exists := row["success"]; exists {
				key := "incompleteCount"
				if metric(success) == 1 {
					key = "successCount"
				}
				n, _ := integer(item[key])
				item[key] = n + 1
			}
		}
		variantList := []Object{}
		for _, v := range variants {
			variantList = append(variantList, v)
		}
		sort.Slice(variantList, func(i, j int) bool {
			a, b := variantList[i], variantList[j]
			if metric(a["lastRunAt"]) != metric(b["lastRunAt"]) {
				return metric(a["lastRunAt"]) > metric(b["lastRunAt"])
			}
			if text(a["family"]) != text(b["family"]) {
				return text(a["family"]) < text(b["family"])
			}
			return text(a["id"]) < text(b["id"])
		})
		sort.Slice(group.runs, func(i, j int) bool {
			a, b := group.runs[i], group.runs[j]
			if metric(a["sampled"]) != metric(b["sampled"]) {
				return metric(a["sampled"]) > metric(b["sampled"])
			}
			return text(a["token"]) < text(b["token"])
		})
		members, memberPage, e := profileMemberPage(group, options)
		if e != nil {
			return e
		}
		runs := []Object{}
		for _, row := range group.runs[:min(30, len(group.runs))] {
			runs = append(runs, Object{"id": row["token"], "family": row["family"], "variant": row["variant"], "machine": row["machine"], "sampledAt": row["sampled"], "memoryBytes": row["memory"], "cpuCores": row["cpu"], "cpuSeconds": row["cpu_seconds"], "durationSeconds": row["duration"], "success": metric(row["success"]) != 0})
		}
		var prediction Object
		var hashedMachine any
		if machine != "" {
			hashedMachine = profileDigest([]any{"machine-v1", machine})
			prediction, e = profilePrediction(c, text(originalGroup.members[0]["family"]), profileDigest([]any{"group-estimate-v1", id}), text(hashedMachine))
			if e != nil {
				return e
			}
		}
		revision, e := profileRevision(c)
		if e != nil {
			return e
		}
		result = Object{"revision": revision, "profile": summary, "members": members, "memberPage": memberPage, "variants": variantList[:min(100, len(variantList))], "runs": runs, "prediction": prediction, "predictionMachine": hashedMachine, "truncated": Object{"members": metric(memberPage["total"]) > float64(len(members)), "variants": len(variantList) > 100, "runs": len(group.runs) > 30}}
		if options.Family != "" {
			result["inspectedFamily"] = options.Family
		}
		return nil
	})
	return result, e
}

func profileMemberPage(group *profileGroup, options ProfileDetailOptions) ([]Object, Object, error) {
	byFamily := map[string]*profileGroup{}
	for _, member := range group.members {
		byFamily[text(member["family"])] = &profileGroup{members: []Object{member}}
	}
	for _, row := range group.runs {
		family := text(row["family"])
		byFamily[family].runs = append(byFamily[family].runs, row)
	}
	for _, row := range group.envelopes {
		family := text(row["family"])
		byFamily[family].envelopes = append(byFamily[family].envelopes, row)
	}
	needle := cases.Fold().String(strings.TrimSpace(options.MemberQuery))
	matching := []Object{}
	for _, member := range group.members {
		if needle == "" || strings.Contains(cases.Fold().String(strings.Join([]string{profileLabel(text(member["family"]), member), text(member["project"]), text(member["executable"]), text(member["family"])}, " ")), needle) {
			matching = append(matching, member)
		}
	}
	start := min(options.MemberOffset, len(matching))
	end := start + min(options.MemberLimit, len(matching)-start)
	result := []Object{}
	for _, member := range matching[start:end] {
		family := text(member["family"])
		summary, e := profileSummary(Object{"id": family}, byFamily[family])
		if e != nil {
			return nil, nil, e
		}
		measurements := Object{}
		for _, key := range []string{"variantCount", "successCount", "incompleteCount", "peakMemoryBytes", "peakCpuCores", "lastRunAt"} {
			measurements[key] = summary[key]
		}
		result = append(result, Object{"family": family, "label": profileLabel(family, member), "project": text(member["project"]), "executable": text(member["executable"]), "measurements": measurements})
	}
	return result, Object{"total": len(matching), "offset": options.MemberOffset, "limit": options.MemberLimit, "query": options.MemberQuery}, nil
}
func profileExpectedRevision(c profileSQL, revision int) error {
	if revision < 0 {
		return fmt.Errorf("Invalid expected learning revision")
	}
	current, e := profileRevision(c)
	if e != nil {
		return e
	}
	if current != revision {
		return fmt.Errorf("Learning changed since you opened it; refresh before saving")
	}
	return nil
}
func profileChanged(c profileSQL) error {
	return profileExec(c, "UPDATE learning_state SET revision=revision+1 WHERE id=1")
}
func profileEditable(c profileSQL, id string) (Object, *profileGroup, error) {
	if e := profileID(id); e != nil {
		return nil, nil, e
	}
	profiles, groups, e := profileDataset(c)
	if e != nil {
		return nil, nil, e
	}
	profile, group := profiles[id], groups[id]
	if profile == nil {
		return nil, nil, fmt.Errorf("Workload profile was not found; refresh the list")
	}
	for _, member := range group.members {
		if _, e = profileRegister(c, text(member["family"]), nil, true); e != nil {
			return nil, nil, e
		}
	}
	return profile, group, nil
}
func (s *ProfileStore) Edit(id string, revision int, label string, memoryFloor, cpuFloor any) error {
	if _, e := profileText(label, "profile label", false, 120, false); e != nil {
		return e
	}
	if memoryFloor != nil {
		n, ok := integer(memoryFloor)
		if !ok || n <= 0 || n > 1<<50 {
			return fmt.Errorf("Minimum memory must be a positive integer no greater than 2^50 bytes")
		}
		memoryFloor = n
	}
	if cpuFloor != nil {
		n, e := profileNumber(cpuFloor, "minimumCpuCores")
		if e != nil || n <= 0 || n > 1024 {
			return fmt.Errorf("Minimum CPU must be greater than zero and no greater than 1024 cores")
		}
		cpuFloor = n
	}
	return s.connection(true, func(c profileSQL) error {
		if e := profileExpectedRevision(c, revision); e != nil {
			return e
		}
		if _, _, e := profileEditable(c, id); e != nil {
			return e
		}
		if e := profileExec(c, "UPDATE learning_profiles SET label=?,memory_floor=?,cpu_floor=?,pinned=1,updated=? WHERE id=?", label, memoryFloor, cpuFloor, resourceNow(), id); e != nil {
			return e
		}
		return profileChanged(c)
	})
}
func (s *ProfileStore) Merge(id, target string, revision int) error {
	if e := profileID(id); e != nil {
		return e
	}
	if e := profileID(target); e != nil {
		return e
	}
	if id == target {
		return fmt.Errorf("Choose a different profile to share learning with")
	}
	return s.connection(true, func(c profileSQL) error {
		if e := profileExpectedRevision(c, revision); e != nil {
			return e
		}
		source, _, e := profileEditable(c, id)
		if e != nil {
			return e
		}
		destination, group, e := profileEditable(c, target)
		if e != nil {
			return e
		}
		if e = profileExec(c, "UPDATE learning_members SET profile=? WHERE profile=?", target, id); e != nil {
			return e
		}
		memory, cpu := max(metric(source["memory_floor"]), metric(destination["memory_floor"])), max(metric(source["cpu_floor"]), metric(destination["cpu_floor"]))
		var memoryValue, cpuValue any
		if memory > 0 {
			memoryValue = int64(memory)
		}
		if cpu > 0 {
			cpuValue = cpu
		}
		ignore := 0
		if metric(source["ignore_legacy"]) != 0 && metric(destination["ignore_legacy"]) != 0 {
			ignore = 1
		}
		if e = profileExec(c, "UPDATE learning_profiles SET label=?,memory_floor=?,cpu_floor=?,ignore_legacy=?,pinned=1,updated=? WHERE id=?", profileGroupLabel(destination, group.members), memoryValue, cpuValue, ignore, resourceNow(), target); e != nil {
			return e
		}
		if e = profileExec(c, "DELETE FROM learning_profiles WHERE id=?", id); e != nil {
			return e
		}
		return profileChanged(c)
	})
}
func (s *ProfileStore) Split(id, family string, revision int) (string, error) {
	if e := profileID(family); e != nil {
		return "", e
	}
	newID := ""
	e := s.connection(true, func(c profileSQL) error {
		if e := profileExpectedRevision(c, revision); e != nil {
			return e
		}
		source, group, e := profileEditable(c, id)
		if e != nil {
			return e
		}
		found := false
		for _, member := range group.members {
			found = found || text(member["family"]) == family
		}
		if !found {
			return fmt.Errorf("That job family does not belong to this profile")
		}
		if len(group.members) < 2 {
			return fmt.Errorf("This job family already has an independent profile")
		}
		newID = family
		existing, e := profileRow(c, "SELECT 1 FROM learning_profiles WHERE id=?", newID)
		if e != nil {
			return e
		}
		if existing != nil {
			newID = profileDigest([]any{"profile-v1", family, token()})
		}
		if e = profileExec(c, "INSERT INTO learning_profiles VALUES (?,?,?,?,1,?,?)", newID, nil, source["memory_floor"], source["cpu_floor"], source["ignore_legacy"], resourceNow()); e != nil {
			return e
		}
		if e = profileExec(c, "UPDATE learning_members SET profile=? WHERE family=?", newID, family); e != nil {
			return e
		}
		if e = profileExec(c, "UPDATE learning_profiles SET pinned=1,updated=? WHERE id=?", resourceNow(), id); e != nil {
			return e
		}
		return profileChanged(c)
	})
	return newID, e
}
func (s *ProfileStore) Reset(id string, revision int) error {
	return s.connection(true, func(c profileSQL) error {
		if e := profileExpectedRevision(c, revision); e != nil {
			return e
		}
		_, group, e := profileEditable(c, id)
		if e != nil {
			return e
		}
		for _, member := range group.members {
			for _, table := range []string{"runs", "envelopes", "learning_retained"} {
				if e = profileExec(c, "DELETE FROM "+table+" WHERE family=?", member["family"]); e != nil {
					return e
				}
			}
			if e = profileExec(c, "INSERT INTO learning_resets VALUES (?,?) ON CONFLICT(family) DO UPDATE SET sampled=MAX(sampled,excluded.sampled)", member["family"], resourceNow()); e != nil {
				return e
			}
		}
		if e = profileExec(c, "UPDATE learning_profiles SET ignore_legacy=1,pinned=1,updated=? WHERE id=?", resourceNow(), id); e != nil {
			return e
		}
		return profileChanged(c)
	})
}
