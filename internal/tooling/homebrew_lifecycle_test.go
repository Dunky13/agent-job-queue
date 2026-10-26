package tooling

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Dunky13/agent-job-queue/internal/backend"
)

// Exercise actual Homebrew delivery movement and rollback, with executable
// sentinels that fail if delivery attempts setup or any runtime operation.
func TestHomebrewNativeAppLifecycle(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Homebrew app lifecycle requires macOS")
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		t.Skip("Homebrew is unavailable")
	}
	for _, prefix := range []string{"opt/homebrew", "usr/local", "custom/prefix", "historical"} {
		t.Run(prefix, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(root, "User With Spaces")
			if err := os.Mkdir(home, 0700); err != nil {
				t.Fatal(err)
			}
			for build := 1; build <= 2; build++ {
				commit := strings.Repeat(strconv.Itoa(build), 40)
				cask, err := RenderCask(backend.Manifest{BuildInfo: backend.BuildInfo{SchemaVersion: 1, Repository: backend.ReleaseRepository, Version: "0.1.0", Commit: &commit, Build: &build}, Tag: "build-" + strconv.Itoa(build) + "-" + commit[:7], Asset: "Agent-Job-Queue-Installer-0.1.0-universal.zip", SHA256: strings.Repeat("a", 64)})
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, strconv.Itoa(build), "agent-job-queue.rb")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(cask), 0600); err != nil {
					t.Fatal(err)
				}
			}
			script := filepath.Join(root, "lifecycle.rb")
			if err := os.WriteFile(script, []byte(homebrewLifecycleRuby), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, brew, "ruby", "--", script, root, prefix)
			command.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_INSTALL_FROM_API=1", "HOMEBREW_DEVELOPER=1"}
			for _, key := range []string{"USER", "LOGNAME", "TMPDIR", "LANG"} {
				if value := os.Getenv(key); value != "" {
					command.Env = append(command.Env, key+"="+value)
				}
			}
			output, err := command.CombinedOutput()
			if err != nil || !strings.Contains(string(output), "installer delivery, rollback, runtime preservation and uninstall verified") {
				t.Fatalf("Homebrew lifecycle failed: %v\n%s", err, output)
			}
		})
	}
}

const homebrewLifecycleRuby = `require "cask/cask_loader"
require "cask/caskroom"
require "cask/installer"
require "find"
require File.expand_path("../../packaging/homebrew/test-cask.rb")
fixture, prefix = Pathname(ARGV.fetch(0)), ARGV.fetch(1)
home = fixture/"User With Spaces"
raise "Unexpected test home" unless Pathname(Dir.home) == home
caskroom = fixture/prefix/"Caskroom"
FileUtils.mkdir_p(caskroom)
FileUtils.chmod(0o775, [caskroom.parent, caskroom])
shared_modes = [caskroom.parent, caskroom].map { |path| path.stat.mode & 0o777 }
Cask::Caskroom.instance_variable_set(:@path, caskroom)

def load_cask(fixture, build)
  cask = Cask::CaskLoader.load(fixture/build.to_s/"agent-job-queue.rb")
  cask.config = Cask::Config.new(explicit: { appdir: (fixture/"Ignored custom appdir").to_s })
  cask.config.instance_variable_set(:@binarydir, fixture/"bin")
  verify_delivery_cask_contract(cask)
  cask
end

def stage_installer(cask, build, historical)
  source = cask.artifacts.grep(Cask::Artifact::App).first.source
  payload = source/"Contents/Resources/payload"
  helper_dir = historical ? payload/"Agent Queue.app/Contents/Resources/payload/source/bin" : payload/"Agent Queue.app/Contents/MacOS"
  forbidden = "#!/bin/sh\nprintf 'forbidden execution: %s\\n' \"$*\" >> \"$HOME/forbidden-execution\"\nexit 95\n"
  [source/"Contents/MacOS/Installer", payload/"source/bin/agent-heavy", payload/"source/bin/agent-remote",
   payload/"Agent Queue.app/Contents/MacOS/AgentQueue", helper_dir/"agent-heavy", helper_dir/"agent-remote"].each do |path|
    path.dirname.mkpath
    path.write(forbidden)
    path.chmod(0o755)
  end
  (source/"build").write(build.to_s)
  %w[agent-heavy agent-remote].each do |name|
    path = payload/"source/links"/name
    path.dirname.mkpath
    FileUtils.cp(File.expand_path("../../packaging/links/#{name}"), path)
  end
  source
end

def verify_links(cask, fixture)
  cask.artifacts.grep(Cask::Artifact::Binary).each do |binary|
    raise "CLI target escaped private prefix" unless binary.target.dirname == fixture/"bin"
    raise "CLI link lost delivered installer source" unless binary.target.symlink? && binary.target.realpath == binary.source.realpath
  end
end

runtime_app = home/"Applications/Agent Queue.app"
private_files = [runtime_app/"Contents/MacOS/AgentQueue", runtime_app/"Contents/MacOS/agent-heavy",
                 home/".local/share/agent-heavy/installation.json", home/".local/state/agent-heavy/queue.sqlite3",
                 home/".config/agent-heavy/credentials", home/".local/bin/agent-heavy",
                 home/"Library/LaunchAgents/io.went.agent-queue.plist", home/"Library/LaunchAgents/io.went.agent-heavy.plist",
                 home/"Library/LaunchAgents/io.went.agent-remote.plist"]
private_files.each do |path|
  path.dirname.mkpath
  path.write("preserved runtime sentinel: #{path.basename}")
end
witness = private_files.to_h { |path| [path, [path.binread, path.stat.ino, path.stat.mode]] }
verify_runtime = lambda do
  witness.each { |path, expected| raise "Delivery moved/deleted/changed runtime #{path}" unless path.exist? && [path.binread, path.stat.ino, path.stat.mode] == expected }
  raise "Delivery executed setup, queue or native command" if (home/"forbidden-execution").exist?
end
historical = prefix == "historical"
old = load_cask(fixture, 1)
old_source = stage_installer(old, 1, historical)
old.metadata_main_container_path.mkpath
old_installer = Cask::Installer.new(old, upgrade: true)
old_installer.install_artifacts
app = old.artifacts.grep(Cask::Artifact::App).first
raise "Installer was not relocated" unless app.source.symlink? && (app.target/"build").read == "1"
verify_links(old, fixture)
verify_runtime.call

# A later native artifact failure must roll back delivery alone.
new = load_cask(fixture, 2)
stage_installer(new, 2, historical)
new.artifacts.grep(Cask::Artifact::Binary).last.define_singleton_method(:install_phase) { |**_options| raise "injected private binary-link failure" }
old_installer.uninstall_artifacts(successor: new, quit: false)
raise "Old installer was not backed up" unless old_source.directory? && !old_source.symlink?
verify_runtime.call
begin
  Cask::Installer.new(new, upgrade: true).install_artifacts(predecessor: old)
  raise "Delivery failure was swallowed"
rescue RuntimeError => error
  raise unless error.message == "injected private binary-link failure"
  raise "Rollback left replacement installer" if app.target.exist?
end
old_installer.install_artifacts(predecessor: new)
raise "Rollback lost old installer" unless (app.target/"build").read == "1"
verify_links(old, fixture)
verify_runtime.call

new = load_cask(fixture, 2)
stage_installer(new, 2, historical)
new_installer = Cask::Installer.new(new, upgrade: true)
old_installer.uninstall_artifacts(successor: new, quit: false)
new_installer.install_artifacts(predecessor: old)
raise "Delivery upgrade did not replace installer" unless (app.target/"build").read == "2"
verify_links(new, fixture)
verify_runtime.call
new_installer.uninstall_artifacts(quit: false)
raise "Uninstall left installer delivery" if app.target.exist?
raise "Uninstall left CLI delivery links" if %w[agent-heavy agent-remote].any? { |name| (fixture/"bin"/name).symlink? }
verify_runtime.call
raise "Delivery changed shared permissions" unless [caskroom.parent, caskroom].map { |path| path.stat.mode & 0o777 } == shared_modes
puts "installer delivery, rollback, runtime preservation and uninstall verified"
`

func TestPackagedHomebrewLinksUseGeneratedCanonicalWrappers(t *testing.T) {
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			generated := filepath.Join(home, ".local/bin", name)
			verificationWrite(t, generated, "#!/bin/sh\nprintf '%s\\n' \"$@\" \"$PACKAGED_TEST_VALUE\"\nexit 7\n", 0700)
			wrapper, err := filepath.Abs(filepath.Join("../../packaging/links", name))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, wrapper, "argument with spaces", "--flag")
			command.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "PACKAGED_TEST_VALUE=literal;$HOME"}
			output, err := command.CombinedOutput()
			if command.ProcessState == nil || command.ProcessState.ExitCode() != 7 || string(output) != "argument with spaces\n--flag\nliteral;$HOME\n" {
				t.Fatalf("canonical wrapper forwarding failed: %v: %s", err, output)
			}
		})
	}
}
