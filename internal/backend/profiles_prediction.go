package backend

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

func profilePredictionGroup(c profileSQL, family string) ([]string, Object, error) {
	has, e := hasProfileLearning(c)
	if e != nil {
		return nil, nil, e
	}
	if !has {
		return []string{family}, Object{}, nil
	}
	member, e := profileRow(c, "SELECT profile FROM learning_members WHERE family=?", family)
	if e != nil {
		return nil, nil, e
	}
	if member == nil {
		return []string{family}, Object{}, nil
	}
	operator, e := profileRow(c, "SELECT * FROM learning_profiles WHERE id=?", member["profile"])
	if e != nil {
		return nil, nil, e
	}
	if operator == nil {
		return nil, nil, fmt.Errorf("Workload grouping is inconsistent")
	}
	rows, e := profileRows(c, "SELECT family FROM learning_members WHERE profile=? ORDER BY family", member["profile"])
	if e != nil {
		return nil, nil, e
	}
	families := []string{}
	for _, row := range rows {
		families = append(families, text(row["family"]))
	}
	return families, operator, nil
}
func (s *ProfileStore) Prediction(identity Object, machine string) (Object, error) {
	if !validProfileIdentity(identity) {
		return nil, fmt.Errorf("Invalid workload identity")
	}
	if machine == "" {
		return nil, fmt.Errorf("A prediction needs a machine identity")
	}
	if s.empty {
		return Object{"memoryBytes": GiB, "cpuCores": 2.0, "durationSeconds": nil, "samples": 0, "confidence": "unknown", "source": "prior"}, nil
	}
	var result Object
	e := s.connection(false, func(c profileSQL) error {
		var err error
		result, err = profilePrediction(c, text(identity["family"]), text(identity["variant"]), profileDigest([]any{"machine-v1", machine}))
		return err
	})
	return result, e
}
func profilePrediction(c profileSQL, family, variant, machine string) (Object, error) {
	families, operator, e := profilePredictionGroup(c, family)
	if e != nil {
		return nil, e
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(families)), ",")
	args := []any{machine}
	for _, f := range families {
		args = append(args, f)
	}
	envelopes, e := profileRows(c, "SELECT * FROM envelopes WHERE machine=? AND family IN ("+placeholders+")", args...)
	if e != nil {
		return nil, e
	}
	has, e := hasProfileLearning(c)
	if e != nil {
		return nil, e
	}
	if has {
		retained, e := profileRows(c, "SELECT * FROM learning_retained WHERE machine=? AND family IN ("+placeholders+")", args...)
		if e != nil {
			return nil, e
		}
		envelopes = append(envelopes, retained...)
	}
	runs, e := profileRows(c, "SELECT * FROM runs WHERE machine=? AND family=? AND variant=? AND sampled>=?", machine, family, variant, resourceNow()-90*profileDay)
	if e != nil {
		return nil, e
	}
	floors := []Object{}
	if metric(operator["ignore_legacy"]) == 0 {
		floors, e = profileRows(c, "SELECT memory,cpu FROM floors WHERE machine IN (?, '*')", machine)
		if e != nil {
			return nil, e
		}
	}
	return profileEstimate(family, variant, operator, envelopes, runs, floors), nil
}

// profileEstimate is shared by individual reads and a collection's batched snapshot.
func profileEstimate(family, variant string, operator Object, envelopes, runs, floors []Object) Object {
	now := resourceNow()
	exact := []Object{}
	sibling := false
	for _, row := range envelopes {
		if text(row["family"]) == family && text(row["variant"]) == variant {
			exact = append(exact, row)
		} else {
			sibling = true
		}
	}
	samples, recent := 0, 0
	var latest Object
	for _, row := range runs {
		if metric(row["success"]) != 0 {
			samples++
			if metric(row["sampled"]) >= now-30*profileDay {
				recent++
			}
		}
		if latest == nil || metric(row["sampled"]) > metric(latest["sampled"]) {
			latest = row
		}
	}
	borrowing := len(exact) > 0 && recent < 5 && sibling
	useExact := len(exact) > 0 && !borrowing
	selected := envelopes
	if !borrowing && len(exact) > 0 {
		selected = exact
	}
	source := "prior"
	if useExact {
		source = "variant"
	} else if len(envelopes) > 0 {
		source = "family"
	}
	minimumMemory, minimumCPU := metric(operator["memory_floor"]), metric(operator["cpu_floor"])
	memoryFloor, cpuFloor := max(float64(GiB), minimumMemory), max(2.0, minimumCPU)
	for _, row := range floors {
		memoryFloor = max(memoryFloor, metric(row["memory"]))
		cpuFloor = max(cpuFloor, metric(row["cpu"]))
	}
	if len(selected) == 0 {
		return Object{"memoryBytes": profileCeilInteger(memoryFloor), "cpuCores": cpuFloor, "durationSeconds": nil, "samples": 0, "confidence": "unknown", "source": source}
	}
	age, maxMemory, maxCPU, maxDuration := 0.0, 0.0, 0.0, 0.0
	latestAt := 0.0
	for _, row := range selected {
		latestAt = max(latestAt, metric(row["sampled"]))
		maxMemory = max(maxMemory, metric(row["memory"]))
		maxCPU = max(maxCPU, metric(row["cpu"]))
		maxDuration = max(maxDuration, metric(row["duration"]))
	}
	age = max(0.0, now-latestAt)
	margin := 1.5
	if useExact {
		margin = 1.25
	}
	factor := max(.75, 1-max(0.0, age-30*profileDay)/(240*profileDay))
	memory, cpu := maxMemory*margin*factor, maxCPU*margin*factor
	if !useExact || recent < 5 {
		memory = max(memory, memoryFloor)
		cpu = max(cpu, cpuFloor)
	} else {
		memory = max(memory, 64*1024*1024, minimumMemory)
		cpu = max(cpu, .1, minimumCPU)
		for _, row := range floors {
			memory = max(memory, metric(row["memory"]))
			cpu = max(cpu, metric(row["cpu"]))
		}
	}
	confidence := "low"
	if useExact && recent >= 12 {
		confidence = "high"
	} else if useExact && recent >= 5 {
		confidence = "medium"
	}
	if age > 30*profileDay || (latest != nil && metric(latest["success"]) == 0) {
		confidence = "low"
	}
	var duration any
	if maxDuration > 0 {
		duration = maxDuration * margin
	}
	return Object{"memoryBytes": profileCeilInteger(memory), "cpuCores": max(profileRoundedCPU(cpu), minimumCPU), "durationSeconds": duration, "samples": samples, "confidence": confidence, "source": source}
}
func (s *ProfileStore) Record(runToken string, identity Object, machine string, metrics Object, complete bool, exitCode *int) error {
	if metrics == nil {
		return fmt.Errorf("Invalid resource metrics")
	}
	if !validProfileIdentity(identity) {
		return fmt.Errorf("Invalid workload identity")
	}
	if runToken == "" || machine == "" {
		return fmt.Errorf("A resource sample needs a token and machine identity")
	}
	values := map[string]float64{}
	for _, key := range []string{"memoryBytes", "cpuSeconds", "durationSeconds", "cpuCores", "sampledAt"} {
		value, exists := metrics[key]
		if !exists {
			value = 0
			if key == "sampledAt" {
				value = resourceNow()
			}
		}
		n, e := profileNumber(value, key)
		if e != nil {
			return e
		}
		values[key] = n
	}
	memory := math.Ceil(values["memoryBytes"])
	if memory >= 9223372036854775808 || values["sampledAt"] > resourceNow()+300 {
		return fmt.Errorf("Resource metric is out of range")
	}
	cpu := values["cpuCores"]
	if values["durationSeconds"] > 0 {
		cpu = max(cpu, values["cpuSeconds"]/values["durationSeconds"])
	}
	if math.IsInf(cpu, 0) || math.IsNaN(cpu) {
		return fmt.Errorf("Invalid resource metric: cpuCores")
	}
	family, variant, context := text(identity["family"]), text(identity["variant"]), text(identity["context"])
	hashedToken, hashedMachine := profileDigest([]any{"run-v1", runToken}), profileDigest([]any{"machine-v1", machine})
	success := 0
	if complete && exitCode != nil && *exitCode == 0 && memory > 0 && values["durationSeconds"] > 0 {
		success = 1
	}
	return s.connection(true, func(c profileSQL) error {
		reset, e := profileRow(c, "SELECT sampled FROM learning_resets WHERE family=?", family)
		if e != nil {
			return e
		}
		if reset != nil && values["sampledAt"] <= metric(reset["sampled"]) {
			return nil
		}
		prior, e := profileRow(c, "SELECT family,variant,machine FROM runs WHERE token=?", hashedToken)
		if e != nil {
			return e
		}
		if prior != nil && (text(prior["family"]) != family || text(prior["variant"]) != variant || text(prior["machine"]) != hashedMachine) {
			return fmt.Errorf("A resource token cannot change workload identity")
		}
		if e = profileExec(c, `INSERT INTO runs VALUES (?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(token) DO UPDATE SET memory=MAX(memory,excluded.memory),cpu=MAX(cpu,excluded.cpu),cpu_seconds=MAX(cpu_seconds,excluded.cpu_seconds),duration=MAX(duration,excluded.duration),sampled=CASE WHEN success=1 AND excluded.success=0 THEN sampled ELSE MAX(sampled,excluded.sampled) END,success=MAX(success,excluded.success)`, hashedToken, family, variant, context, hashedMachine, int64(memory), cpu, values["cpuSeconds"], values["durationSeconds"], values["sampledAt"], success); e != nil {
			return e
		}
		if e = profileExec(c, `INSERT INTO envelopes VALUES (?,?,?,?,?,?,?) ON CONFLICT(machine,family,variant) DO UPDATE SET memory=MAX(memory,excluded.memory),cpu=MAX(cpu,excluded.cpu),duration=MAX(duration,excluded.duration),sampled=MAX(sampled,excluded.sampled)`, hashedMachine, family, variant, int64(memory), cpu, values["durationSeconds"], values["sampledAt"]); e != nil {
			return e
		}
		if e = profileExec(c, "DELETE FROM runs WHERE token IN (SELECT token FROM runs ORDER BY sampled DESC LIMIT -1 OFFSET ?)", maxProfileRuns); e != nil {
			return e
		}
		overflow, e := profileRows(c, "SELECT * FROM envelopes ORDER BY sampled DESC LIMIT -1 OFFSET ?", maxProfileEnvelopes)
		if e != nil {
			return e
		}
		for _, row := range overflow {
			if e = profileExec(c, `INSERT INTO learning_retained VALUES (?,?,?,?,?,?,?) ON CONFLICT(machine,family,variant) DO UPDATE SET memory=MAX(memory,excluded.memory),cpu=MAX(cpu,excluded.cpu),duration=MAX(duration,excluded.duration),sampled=MAX(sampled,excluded.sampled)`, row["machine"], row["family"], row["variant"], row["memory"], row["cpu"], row["duration"], row["sampled"]); e != nil {
				return e
			}
			if e = profileExec(c, "DELETE FROM envelopes WHERE machine=? AND family=? AND variant=?", row["machine"], row["family"], row["variant"]); e != nil {
				return e
			}
		}
		overflow, e = profileRows(c, "SELECT * FROM learning_retained ORDER BY sampled DESC LIMIT -1 OFFSET ?", maxRetainedEnvelopes)
		if e != nil {
			return e
		}
		for _, row := range overflow {
			if e = profileExec(c, `INSERT INTO floors VALUES (?,?,?,?) ON CONFLICT(machine) DO UPDATE SET memory=MAX(memory,excluded.memory),cpu=MAX(cpu,excluded.cpu),sampled=MAX(sampled,excluded.sampled)`, row["machine"], row["memory"], row["cpu"], row["sampled"]); e != nil {
				return e
			}
			if e = profileExec(c, "UPDATE learning_profiles SET ignore_legacy=0 WHERE id IN (SELECT profile FROM learning_members WHERE family=?)", row["family"]); e != nil {
				return e
			}
			if e = profileExec(c, "DELETE FROM learning_retained WHERE machine=? AND family=? AND variant=?", row["machine"], row["family"], row["variant"]); e != nil {
				return e
			}
		}
		overflow, e = profileRows(c, "SELECT * FROM floors WHERE machine!='*' ORDER BY sampled DESC LIMIT -1 OFFSET ?", maxProfileMachines)
		if e != nil {
			return e
		}
		for _, row := range overflow {
			if e = profileExec(c, `INSERT INTO floors VALUES ('*',?,?,?) ON CONFLICT(machine) DO UPDATE SET memory=MAX(memory,excluded.memory),cpu=MAX(cpu,excluded.cpu),sampled=MAX(sampled,excluded.sampled)`, row["memory"], row["cpu"], row["sampled"]); e != nil {
				return e
			}
			if e = profileExec(c, "DELETE FROM floors WHERE machine=?", row["machine"]); e != nil {
				return e
			}
		}
		return nil
	})
}

func profileCeilInteger(value float64) any {
	rounded := math.Ceil(value)
	if rounded < 9223372036854775808 {
		return int64(rounded)
	}
	return json.Number(strconv.FormatFloat(rounded, 'f', 0, 64))
}
func profileRoundedCPU(value float64) float64 {
	rounded, e := strconv.ParseFloat(strconv.FormatFloat(value, 'f', 3, 64), 64)
	if e != nil {
		return value
	}
	return rounded
}
