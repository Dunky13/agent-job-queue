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
	for _, line := range strings.Split(string(batch), "\n") {
		if m := regexp.MustCompile(`^bash scripts/test-go-runtime\.sh (\S+)$`).FindStringSubmatch(line); m != nil {
			scopes = append(scopes, m[1])
		}
		if m := regexp.MustCompile(`^bash scripts/test-go-tooling\.sh (\d+)$`).FindStringSubmatch(line); m != nil {
			shard, _ := strconv.Atoi(m[1])
			shards = append(shards, shard)
		}
		if regexp.MustCompile(`^go test .*(\./internal/|\./cmd/)`).MatchString(line) {
			t.Fatalf("validate.sh must use the partition drivers, not %q", line)
		}
	}
	if want := workflow.Jobs["macos-tests"].Strategy.Matrix.Scope; len(want) == 0 || !reflect.DeepEqual(scopes, want) {
		t.Fatalf("validate.sh runtime partitions %v differ from CI %v", scopes, want)
	}
	if want := workflow.Jobs["tooling-tests"].Strategy.Matrix.Shard; len(want) == 0 || !reflect.DeepEqual(shards, want) {
		t.Fatalf("validate.sh tooling shards %v differ from CI %v", shards, want)
	}
}
