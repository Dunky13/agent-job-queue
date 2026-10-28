package backend

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Historical execution payloads are disposable. Join retained numeric runs to
// their private safe metadata by hashed token, without retaining raw commands.
func (s *ProfileStore) historicalSimilarityDescriptions(g *Gate) (map[string]Object, error) {
	tokens := map[string]string{}
	if !s.empty {
		if e := s.connection(false, func(c profileSQL) error {
			rows, e := profileRows(c, "SELECT token,family FROM runs")
			for _, row := range rows {
				tokens[text(row["token"])] = text(row["family"])
			}
			return e
		}); e != nil {
			return nil, e
		}
	}
	info, e := os.Lstat(g.Metadata)
	if os.IsNotExist(e) {
		return map[string]Object{}, nil
	}
	if e != nil {
		return nil, e
	}
	stat, owned := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || !owned || int(stat.Uid) != os.Getuid() {
		return nil, fmt.Errorf("Unsafe workload metadata directory")
	}
	entries, e := os.ReadDir(g.Metadata)
	if e != nil {
		return nil, e
	}
	descriptions := map[string]Object{}
	conflicts := map[string]bool{}
	scopes := map[string]string{}
	for _, entry := range entries {
		if e := g.operationContext().Err(); e != nil {
			return nil, e
		}
		value := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || !validToken(value) {
			continue
		}
		family := tokens[profileDigest([]any{"run-v1", value})]
		if family == "" {
			identity, e := SchedulingIdentity(g, value)
			if e != nil {
				return nil, e
			}
			family = text(identity["family"])
		}
		if family == "" || conflicts[family] {
			continue
		}
		description, e := ReadPrivate(filepath.Join(g.Metadata, entry.Name()))
		if os.IsNotExist(e) {
			continue // A concurrent normal history cleanup may retire a file.
		}
		if e != nil {
			return nil, e
		}
		clean := profileSimilarityDescription(description, scopes)
		if clean["learningScope"] == nil {
			continue
		}
		if prior := descriptions[family]; prior != nil && (prior["learningScope"] != clean["learningScope"] || prior["learningKind"] != clean["learningKind"]) {
			delete(descriptions, family)
			conflicts[family] = true
			continue
		}
		descriptions[family] = clean
	}
	return descriptions, nil
}

func profileRegroupPlan(c profileSQL, descriptions map[string]Object) (Object, error) {
	profiles, groups, e := profileDataset(c)
	if e != nil {
		return nil, e
	}
	signatures := map[string]Object{}
	for family, description := range descriptions {
		signatures[family] = description
	}
	has, e := hasProfileSimilarity(c)
	if e != nil {
		return nil, e
	}
	if has {
		rows, e := profileRows(c, "SELECT * FROM learning_similarity")
		if e != nil {
			return nil, e
		}
		for _, row := range rows {
			if !profileHash.MatchString(text(row["scope"])) || !validProfileSimilarityKind(text(row["kind"])) {
				return nil, fmt.Errorf("Invalid stored workload similarity")
			}
			signatures[text(row["family"])] = Object{"learningScope": row["scope"], "learningKind": row["kind"]}
		}
	}
	compatible := map[string][]Object{}
	for id, group := range groups {
		if metric(profiles[id]["pinned"]) != 0 {
			continue
		}
		key := ""
		valid := true
		for _, member := range group.members {
			signature := signatures[text(member["family"])]
			scope, kind := text(signature["learningScope"]), text(signature["learningKind"])
			candidate := scope + ":" + kind
			if !profileHash.MatchString(scope) || !validProfileSimilarityKind(kind) || (key != "" && candidate != key) {
				valid = false
				break
			}
			key = candidate
		}
		if valid && key != "" {
			compatible[key] = append(compatible[key], profiles[id])
		}
	}
	keys := []string{}
	for key := range compatible {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	merges, families := 0, 0
	proposed := []Object{}
	for _, key := range keys {
		entries := compatible[key]
		if len(entries) < 2 {
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return text(entries[i]["id"]) < text(entries[j]["id"]) })
		target := text(entries[0]["id"])
		members, successes, incomplete := 0, 0, 0
		for _, entry := range entries {
			group := groups[text(entry["id"])]
			members += len(group.members)
			for _, run := range group.runs {
				if metric(run["success"]) == 1 {
					successes++
				} else {
					incomplete++
				}
			}
		}
		member := groups[target].members[0]
		kind := text(signatures[text(member["family"])]["learningKind"])
		proposed = append(proposed, Object{"id": target, "label": profileSimilarityLabel(kind, text(member["project"])), "memberCount": members, "successCount": successes, "incompleteCount": incomplete})
		merges += len(entries) - 1
		families += members
	}
	revision, e := profileRevision(c)
	if e != nil {
		return nil, e
	}
	return Object{"revision": revision, "profilesBefore": len(profiles), "profilesAfter": len(profiles) - merges, "familiesGrouped": families, "groups": proposed, "applied": false}, nil
}

func (s *ProfileStore) regroup(descriptions map[string]Object, revision int, apply bool) (Object, error) {
	if s.empty {
		return Object{"revision": 0, "profilesBefore": 0, "profilesAfter": 0, "familiesGrouped": 0, "groups": []Object{}, "applied": false}, nil
	}
	var result Object
	e := s.connection(apply, func(c profileSQL) error {
		if apply {
			if e := profileExpectedRevision(c, revision); e != nil {
				return e
			}
		}
		var e error
		result, e = profileRegroupPlan(c, descriptions)
		if e != nil || !apply {
			return e
		}
		profiles, groups, e := profileDataset(c)
		if e != nil {
			return e
		}
		families := map[string]bool{}
		for id := range profiles {
			for _, member := range groups[id].members {
				families[text(member["family"])] = true
			}
		}
		keys := []string{}
		for family := range descriptions {
			if families[family] {
				keys = append(keys, family)
			}
		}
		sort.Strings(keys)
		for _, family := range keys {
			clean := descriptions[family]
			member, e := profileRegister(c, family, clean, true)
			if e != nil {
				return e
			}
			if member == nil {
				return fmt.Errorf("Historical workload registration failed")
			}
			if e = profileExec(c, "INSERT OR IGNORE INTO learning_similarity VALUES (?,?,?)", family, clean["learningScope"], clean["learningKind"]); e != nil {
				return e
			}
		}
		changed, e := profileGroupSimilar(c)
		if e != nil {
			return e
		}
		if changed {
			if e = profileChanged(c); e != nil {
				return e
			}
		}
		result["revision"], e = profileRevision(c)
		result["applied"] = true
		return e
	})
	return result, e
}

func LearningRegroup(g *Gate, apply bool, request Object) (Object, error) {
	store, e := NewProfileStore(g.State, true)
	if e != nil {
		return nil, e
	}
	descriptions, e := store.historicalSimilarityDescriptions(g)
	if e != nil {
		return nil, e
	}
	if !apply {
		return store.regroup(descriptions, 0, false)
	}
	revision, ok := schedulingInteger(request["revision"])
	if !ok || revision < 0 || len(request) != 1 {
		return nil, fmt.Errorf("Regroup requires the reviewed learning revision")
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
	store, e = NewProfileStore(g.State, false)
	if e != nil {
		return nil, e
	}
	if current, e := store.Revision(); e != nil || current != revision {
		if e != nil {
			return nil, e
		}
		return nil, fmt.Errorf("Learning changed since you opened it; refresh before saving")
	}
	// Rotate grants through the same guarded path as an operator correction.
	// Active ownership and manual overrides survive; pending grants do not.
	plan, e := store.regroup(descriptions, revision, false)
	if e != nil {
		return nil, e
	}
	if plan["profilesBefore"] != plan["profilesAfter"] {
		if e = invalidateLearningAdmissions(g); e != nil {
			return nil, e
		}
	}
	return store.regroup(descriptions, revision, true)
}

func (s *ProfileStore) backfillSimilarity(g *Gate) error {
	needed := false
	if e := s.connection(false, func(c profileSQL) error {
		row, e := profileRow(c, "SELECT version FROM learning_similarity_state WHERE id=1")
		needed = metric(row["version"]) < 1
		return e
	}); e != nil || !needed {
		return e
	}
	revision, e := s.Revision()
	if e != nil {
		return e
	}
	_, e = LearningRegroup(g, true, Object{"revision": revision})
	if e != nil {
		return e
	}
	// Only the updated controller marks its backfill complete. An explicit CLI
	// regroup can precede a runtime upgrade while old clients still add families.
	return s.connection(true, func(c profileSQL) error {
		return profileExec(c, "UPDATE learning_similarity_state SET version=1 WHERE id=1")
	})
}
