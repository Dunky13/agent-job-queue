package tooling

import (
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The local batch must run the partitions hosted CI requires. One go test over
// internal/backend or internal/tooling exceeds the per-package 12-minute timeout.
func TestValidateBatchRunsTheCIGoPartitions(t *testing.T) {
	ci, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Shard []int    `yaml:"shard"`
					Scope []string `yaml:"scope"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(ci, &workflow); err != nil {
		t.Fatal(err)
	}
	batch, err := os.ReadFile("../../scripts/validate.sh")
	if err != nil {
		t.Fatal(err)
	}
	var scopes []string
	var shards []int
	runtime := regexp.MustCompile(`^bash scripts/test-go-runtime\.sh (\S+)$`)
	tooling := regexp.MustCompile(`^bash scripts/test-go-tooling\.sh (\d+)$`)
	for _, line := range strings.Split(string(batch), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if m := runtime.FindStringSubmatch(line); m != nil {
			scopes = append(scopes, m[1])
			continue
		}
		if m := tooling.FindStringSubmatch(line); m != nil {
			shard, _ := strconv.Atoi(m[1])
			shards = append(shards, shard)
			continue
		}
		// Any other form, such as a direct go test, a driver with no selector or
		// one wrapped in a condition, can bring back the over-limit single run.
		if strings.Contains(line, "go test") || strings.Contains(line, "test-go-runtime") || strings.Contains(line, "test-go-tooling") {
			t.Fatalf("validate.sh must run each partition as a plain driver line, not %q", line)
		}
	}
	if want := workflow.Jobs["macos-tests"].Strategy.Matrix.Scope; len(want) == 0 || !reflect.DeepEqual(scopes, want) {
		t.Fatalf("validate.sh runtime partitions %v differ from CI %v", scopes, want)
	}
	if want := workflow.Jobs["tooling-tests"].Strategy.Matrix.Shard; len(want) == 0 || !reflect.DeepEqual(shards, want) {
		t.Fatalf("validate.sh tooling shards %v differ from CI %v", shards, want)
	}
}
