package tooling

import (
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

func TestHomebrewAppCaskPreservesQuarantineWithFixtureApp(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Homebrew quarantine delivery requires macOS")
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		t.Skip("Homebrew is unavailable")
	}
	fixture, home, environment := caskFixture(t)
	writeFixtureApp(t, filepath.Join(fixture, "Agent Queue.app"), 123)
	cask := fixtureCask(t, fixture, "0.1.0", 123)
	output, err := runBrewRuby(t, 30*time.Second, environment, brew, "--", "../../packaging/homebrew/test-cask.rb", cask, fixture, "--quarantined")
	if err != nil {
		t.Fatalf("quarantined app delivery failed: %v: %s", err, output)
	}
	assertFixtureDelivery(t, output)
	if _, err := os.Stat(filepath.Join(home, "Applications/Agent Queue.app/Contents/MacOS/agent-heavy")); err != nil {
		t.Fatalf("Homebrew did not place the complete app: %v", err)
	}
	// Placing the app must neither run it nor perform its first-launch setup.
	for _, name := range []string{"forbidden-execution", ".local", "Library/LaunchAgents"} {
		if _, err := os.Stat(filepath.Join(home, name)); !os.IsNotExist(err) {
			t.Fatalf("installation executed or set up the app: %s (%v)", name, err)
		}
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
	root, home, environment := caskFixture(t)
	valid := fixtureCask(t, root, "0.1.0", 123)
	body, err := os.ReadFile(valid)
	if err != nil {
		t.Fatal(err)
	}
	const script = `{ executable: "/bin/sh", args: ["-c", "true"], sudo: false, must_succeed: true }`
	paths := []string{valid}
	for _, mutation := range []struct{ name, before, after string }{
		{"installer-script", "  caveats <<~EOS", "  installer script: " + script + "\n  caveats <<~EOS"},
		{"uninstall-script", `uninstall quit: "io.went.agent-queue"`, "uninstall script: " + script},
		{"uninstall-extra", `uninstall quit: "io.went.agent-queue"`, `uninstall quit: "io.went.agent-queue", delete: "/tmp/unexpected"`},
		{"uninstall-bundle", `quit: "io.went.agent-queue"`, `quit: "io.went.other"`},
		{"preflight", "  caveats <<~EOS", "  preflight_steps do\n    run \"/bin/true\"\n  end\n  caveats <<~EOS"},
		{"postflight", "  caveats <<~EOS", "  postflight_steps do\n    run \"/bin/true\"\n  end\n  caveats <<~EOS"},
		{"removal", "  caveats <<~EOS", "  uninstall_preflight_steps do\n    run \"/bin/true\"\n  end\n  caveats <<~EOS"},
		{"binary", "  caveats <<~EOS", "  binary \"Agent Queue.app/Contents/MacOS/agent-heavy\"\n  caveats <<~EOS"},
		{"app-target", `app "Agent Queue.app"`, `app "Agent Queue.app", target: "#{Dir.home}/Applications/Agent Queue.app"`},
		{"installer-app", `app "Agent Queue.app"`, `app "Agent Job Queue Installer.app"`},
		{"extra-app", "  caveats <<~EOS", "  app \"Other.app\"\n  caveats <<~EOS"},
	} {
		changed := strings.Replace(string(body), mutation.before, mutation.after, 1)
		if changed == string(body) {
			t.Fatalf("mutation %s no longer targets the current cask", mutation.name)
		}
		path := filepath.Join(root, mutation.name, "agent-job-queue.rb")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	driver := filepath.Join(root, "verify-contract.rb")
	if err := os.WriteFile(driver, []byte(`require File.expand_path("../../packaging/homebrew/test-cask.rb")
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
`), 0600); err != nil {
		t.Fatal(err)
	}
	// Keep the literal HOME in the same path shape used by release acceptance.
	if !strings.Contains(home, "User With Spaces") {
		t.Fatal("missing spaced HOME")
	}
	output, err := runBrewRuby(t, 30*time.Second, environment, brew, append([]string{"--", driver}, paths...)...)
	if err != nil || !strings.Contains(string(output), "exact delivery contracts verified") {
		t.Fatalf("cask contract drift check failed: %v: %s", err, output)
	}
}
