package tooling

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestManagedCaskTemplateParity(t *testing.T) {
	source, err := parser.ParseFile(token.NewFileSet(), "../../packaging/homebrew/refresh.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var standalone string
	ast.Inspect(source, func(node ast.Node) bool {
		value, ok := node.(*ast.ValueSpec)
		if !ok || len(value.Names) != 1 || value.Names[0].Name != "caskTemplate" || len(value.Values) != 1 {
			return true
		}
		literal, ok := value.Values[0].(*ast.BasicLit)
		if !ok {
			t.Fatal("standalone cask template is not a string literal")
		}
		standalone, err = strconv.Unquote(literal.Value)
		if err != nil {
			t.Fatal(err)
		}
		return false
	})
	if standalone == "" || standalone != caskTemplate {
		t.Fatal("release and standalone Homebrew lifecycle templates differ")
	}
}

func TestHomebrewDeliveryPreservesQuarantineWithoutExecutingPayload(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Homebrew quarantine delivery requires macOS")
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		t.Skip("Homebrew is unavailable")
	}
	for _, historical := range []bool{false, true} {
		t.Run(strconv.FormatBool(historical), func(t *testing.T) {
			root, home, environment, owner := notarizedCaskFixture(t, "AAAAAAAAAA")
			protocolPath := filepath.Join(root, "fixture-environment.json")
			body, err := os.ReadFile(protocolPath)
			if err != nil {
				t.Fatal(err)
			}
			var protocol map[string]string
			if err := json.Unmarshal(body, &protocol); err != nil {
				t.Fatal(err)
			}
			delete(protocol, "AGENT_QUEUE_INSTALL_TEST_TEAM")
			body, err = json.Marshal(protocol)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(protocolPath, body, 0600); err != nil {
				t.Fatal(err)
			}
			installer := filepath.Join(root, "Agent Job Queue Installer.app")
			payload := filepath.Join(installer, "Contents/Resources/payload")
			paths := []string{filepath.Join(installer, "Contents/MacOS/Installer"), filepath.Join(payload, "source/bin/agent-heavy"), filepath.Join(payload, "source/bin/agent-remote"), filepath.Join(payload, "Agent Queue.app/Contents/MacOS/AgentQueue")}
			helperDir := "Agent Queue.app/Contents/MacOS"
			if historical {
				helperDir = "Agent Queue.app/Contents/Resources/payload/source/bin"
			}
			for _, name := range []string{"agent-heavy", "agent-remote"} {
				paths = append(paths, filepath.Join(payload, helperDir, name), filepath.Join(payload, "source/links", name))
			}
			for _, path := range paths {
				verificationWrite(t, path, "#!/bin/sh\ntouch \"$HOME/forbidden-execution\"\nexit 95\n", 0700)
			}
			runtime := filepath.Join(home, "Applications/Agent Queue.app/Contents/MacOS/AgentQueue")
			verificationWrite(t, runtime, "live runtime sentinel", 0700)
			before, err := os.Stat(runtime)
			if err != nil {
				t.Fatal(err)
			}
			cask := notarizedFixtureCask(t, root, "0.1.0")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command, err := owner.start(ctx, environment, nil, brew, "ruby", "--", "../../packaging/homebrew/test-cask.rb", cask, root, "--quarantined")
			if err != nil {
				t.Fatal(err)
			}
			output, err := command.wait()
			if err != nil {
				t.Fatalf("quarantined installer delivery failed: %v: %s", err, output)
			}
			var result struct {
				Installed bool           `json:"fixture_app_installed"`
				Executed  bool           `json:"fixture_installer_executed"`
				Attrs     map[string]int `json:"fixture_attributes"`
			}
			lines := strings.Split(strings.TrimSpace(string(output)), "\n")
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil || !result.Installed || result.Executed || len(result.Attrs) != 5 || result.Attrs["entries"] == 0 {
				t.Fatalf("invalid delivery evidence: %v: %s", err, output)
			}
			for _, count := range result.Attrs {
				if count != result.Attrs["entries"] {
					t.Fatal("delivery lost quarantine or unrelated xattrs", result.Attrs)
				}
			}
			after, err := os.Stat(runtime)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("delivery replaced the live runtime", err)
			}
			if _, err := os.Stat(filepath.Join(home, "forbidden-execution")); !os.IsNotExist(err) {
				t.Fatal("delivery executed the installer or unsupported historical flags", err)
			}
		})
	}
}

func TestManagedCaskLoaderRejectsHookDrift(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("real Homebrew DSL requires macOS")
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		t.Skip("Homebrew is unavailable")
	}
	root, home, environment, owner := notarizedCaskFixture(t, "AAAAAAAAAA")
	valid := notarizedFixtureCask(t, root, "0.1.0")
	body, err := os.ReadFile(valid)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{valid}
	for _, mutation := range []struct{ name, before, after string }{
		{"runtime-app", `app "Agent Job Queue Installer.app",`, `app "Agent Job Queue Installer.app/Contents/Resources/payload/Agent Queue.app",`},
		{"runtime-target", `Applications/Agent Job Queue Installer.app`, `Applications/Agent Queue.app`},
		{"preflight", "  caveats <<~EOS", "  preflight_steps do\n    run \"/bin/true\"\n  end\n  caveats <<~EOS"},
		{"postflight", "  caveats <<~EOS", "  postflight_steps do\n    run \"/bin/true\"\n  end\n  caveats <<~EOS"},
		{"uninstall", "  caveats <<~EOS", "  uninstall quit: \"io.went.agent-queue\"\n  caveats <<~EOS"},
		{"removal", "  caveats <<~EOS", "  uninstall_preflight_steps do\n    run \"/bin/true\"\n  end\n  caveats <<~EOS"},
		{"binary", "source/links/agent-heavy", "source/bin/agent-heavy"},
		{"binary-target", `source/links/agent-heavy"`, `source/links/agent-heavy", target: "unexpected"`},
		{"extra-app", "  caveats <<~EOS", "  app \"Other.app\"\n  caveats <<~EOS"},
	} {
		changed := strings.Replace(string(body), mutation.before, mutation.after, 1)
		if changed == string(body) {
			t.Fatalf("mutation %s no longer targets the current cask", mutation.name)
		}
		path := filepath.Join(root, mutation.name, "agent-job-queue.rb")
		verificationWrite(t, path, changed, 0600)
		paths = append(paths, path)
	}
	script := filepath.Join(root, "verify-contract.rb")
	verificationWrite(t, script, `require File.expand_path("../../packaging/homebrew/test-cask.rb")
ARGV.each_with_index do |path, index|
  rejected = false
  begin
    cask = Cask::CaskLoader.load(Pathname(path))
    verify_delivery_cask_contract(cask)
  rescue RuntimeError, MethodDeprecatedError
    rejected = true
  end
  raise "Wrong contract verdict for #{path}" unless rejected == (index > 0)
end
puts "exact delivery contracts verified"
`, 0600)
	// Keep the literal HOME in the same path shape used by release acceptance.
	if !strings.Contains(home, "User With Spaces") {
		t.Fatal("missing spaced HOME")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command, err := owner.start(ctx, environment, nil, brew, append([]string{"ruby", "--", script}, paths...)...)
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.wait()
	if err != nil || !strings.Contains(string(output), "exact delivery contracts verified") {
		t.Fatalf("cask contract drift check failed: %v: %s", err, output)
	}
}
