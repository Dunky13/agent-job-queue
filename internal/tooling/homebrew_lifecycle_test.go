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

// Exercise Homebrew's actual App movement and install-artifact rollback. The
// setup executable refuses execution; native first-launch acceptance separately
// runs the real Go installer after Homebrew has finished moving the signed app.
func TestHomebrewNativeAppLifecycle(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Homebrew app lifecycle requires macOS")
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		t.Skip("Homebrew is unavailable")
	}
	for _, prefix := range []string{"opt/homebrew", "usr/local", "custom/prefix"} {
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
			if err != nil || !strings.Contains(string(output), "native app install, rollback, and uninstall verified") {
				t.Fatalf("Homebrew lifecycle failed: %v\n%s", err, output)
			}
		})
	}
}

const homebrewLifecycleRuby = `require "cask/cask_loader"
require "cask/caskroom"
require "cask/installer"

fixture, prefix = Pathname(ARGV.fetch(0)), ARGV.fetch(1)
home = fixture/"User With Spaces"
raise "Unexpected test home" unless Pathname(Dir.home) == home
caskroom = fixture/prefix/"Caskroom"
FileUtils.mkdir_p(caskroom)
FileUtils.chmod(0o775, [caskroom.parent, caskroom])
shared_modes = [caskroom.parent, caskroom].map { |path| path.stat.mode & 0o777 }
Cask::Caskroom.instance_variable_set(:@path, caskroom)

# Every artifact, including real binary symlinks and uninstall scripts, remains
# inside this fixture. quit:false below prevents targeting the operator's app.
def load_cask(fixture, build)
  cask = Cask::CaskLoader.load(fixture/build.to_s/"agent-job-queue.rb")
  cask.config = Cask::Config.new(explicit: { appdir: (fixture/"Ignored custom appdir").to_s })
  cask.config.instance_variable_set(:@binarydir, fixture/"bin")
  apps = cask.artifacts.grep(Cask::Artifact::App)
  raise "Missing native App artifact" unless apps.length == 1
  raise "Custom appdir changed the per-user target" unless apps.first.target == Pathname(Dir.home)/"Applications/Agent Queue.app"
  raise "Fixture binary escaped its private prefix" unless cask.artifacts.grep(Cask::Artifact::Binary).all? { |artifact| artifact.target.dirname == fixture/"bin" }
  forbidden = [Cask::Artifact::Installer, Cask::Artifact::PreflightBlock, Cask::Artifact::PostflightBlock]
  forbidden << Cask::Artifact::PreflightSteps if defined?(Cask::Artifact::PreflightSteps)
  forbidden << Cask::Artifact::PostflightSteps if defined?(Cask::Artifact::PostflightSteps)
  raise "Cask has executable install hooks" if cask.artifacts.any? { |artifact| forbidden.any? { |klass| artifact.is_a?(klass) } }
  cask
end

def stage_app(cask, build)
  app = cask.artifacts.grep(Cask::Artifact::App).fetch(0)
  bin = app.source/"Contents/Resources/payload/source/bin"
  links = app.source.parent/"source/links"
  FileUtils.mkdir_p([bin, links])
  (app.source/"build").write(build.to_s)
  helper = bin/"agent-heavy"
  helper.write("#!/bin/sh\nprintf 'unexpected setup' > \"$HOME/setup-executed\"\nexit 99\n")
  helper.chmod(0o755)
  %w[agent-heavy agent-remote].each do |name|
    wrapper = links/name
    wrapper.write("#!/bin/sh\nexit 99\n")
    wrapper.chmod(0o755)
  end
  app.source
end

def verify_cli_links(cask)
  cask.artifacts.grep(Cask::Artifact::Binary).each do |binary|
    unless binary.target.symlink? && binary.target.realpath == binary.source.realpath
      raise "CLI link does not resolve to its expected cask source: #{binary.target}"
    end
  end
end

state = home/".local/state/agent-heavy/keep"
credentials = home/".config/agent-heavy/keep"
launch_agent = home/"Library/LaunchAgents/io.went.agent-queue.plist"
[state, credentials, launch_agent].each do |path|
  path.dirname.mkpath
  path.write("private sentinel")
end
old = load_cask(fixture, 1)
old_source = stage_app(old, 1)
old_installer = Cask::Installer.new(old, upgrade: true)
old.metadata_main_container_path.mkpath
old_installer.install_artifacts
app = old.artifacts.grep(Cask::Artifact::App).fetch(0)
raise "App remained in shared staging" unless app.source.symlink? && app.target.directory?
raise "Installed app is group-writable" unless (app.target.stat.mode & 0o022).zero?
verify_cli_links(old)

new = load_cask(fixture, 2)
stage_app(new, 2)
new_installer = Cask::Installer.new(new, upgrade: true)
# Fail a real later artifact, after Homebrew has installed the replacement App.
new.artifacts.grep(Cask::Artifact::Binary).last.define_singleton_method(:install_phase) do |**_options|
  raise "injected private binary-link failure"
end
old_installer.uninstall_artifacts(successor: new, quit: false)
raise "Old app was not backed up" unless old_source.directory? && !old_source.symlink? && (!app.target.exist? || app.target.children.empty?)
failed = false
begin
  new_installer.install_artifacts(predecessor: old)
rescue RuntimeError => error
  failed = true
  raise "Unexpected install failure" unless error.message == "injected private binary-link failure"
end
raise "Failed installation was swallowed" unless failed
raise "Failed installation left its replacement app" if app.target.exist?
old_installer.install_artifacts(predecessor: new)
raise "Old app was not restored" unless (app.target/"build").read == "1"
verify_cli_links(old)

launch_agent.write("private sentinel")
old_installer.uninstall_artifacts(quit: false)
raise "Uninstall left the menu app" if app.target.exist?
raise "Uninstall left the app launch agent" if launch_agent.exist?
raise "Uninstall left the CLI links" if %w[agent-heavy agent-remote].any? { |name| (fixture/"bin"/name).symlink? }
raise "Homebrew executed native setup" if (home/"setup-executed").exist?
[state, credentials].each { |path| raise "Lifecycle changed private queue state" unless path.read == "private sentinel" }
raise "Lifecycle changed shared Homebrew permissions" unless [caskroom.parent, caskroom].map { |path| path.stat.mode & 0o777 } == shared_modes
puts "native app install, rollback, and uninstall verified"
`
