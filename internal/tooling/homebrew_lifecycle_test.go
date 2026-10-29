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

// Exercise Homebrew's placement, upgrade, rollback and removal of the app with
// a data-only app. Setup belongs to the app's first launch, so the lifecycle
// must leave every private runtime file exactly as it found it.
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
				cask, err := RenderCask(backend.Manifest{BuildInfo: backend.BuildInfo{SchemaVersion: 1, Repository: backend.ReleaseRepository, Version: "0.1.0", Commit: &commit, Build: &build}, Tag: "build-" + strconv.Itoa(build) + "-" + commit[:7], Asset: "Agent-Queue-0.1.0-universal.zip", SHA256: strings.Repeat("a", 64)})
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
			if err != nil || !strings.Contains(string(output), "app delivery, rollback, runtime preservation and uninstall verified") {
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
  cask.config = Cask::Config.new(explicit: { appdir: (fixture/"User With Spaces/Applications").to_s })
  verify_delivery_cask_contract(cask)
  cask
end

def stage_app(cask, build)
  source = cask.artifacts.grep(Cask::Artifact::App).first.source
  forbidden = "#!/bin/sh\nprintf 'forbidden execution: %s\\n' \"$*\" >> \"$HOME/forbidden-execution\"\nexit 95\n"
  %w[AgentQueue agent-heavy agent-remote].each do |name|
    path = source/"Contents/MacOS"/name
    path.dirname.mkpath
    path.write(forbidden)
    path.chmod(0o755)
  end
  (source/"Contents/Info.plist").write(<<~PLIST)
    <?xml version="1.0"?><plist version="1.0"><dict><key>CFBundleShortVersionString</key><string>0.1.0</string><key>CFBundleVersion</key><string>#{build}</string></dict></plist>
  PLIST
  (source/"build").write(build.to_s)
  source
end

# The app's own setup artifacts. Homebrew must never move, run or delete them.
private_files = [home/".local/bin/agent-heavy", home/".local/bin/agent-remote",
                 home/".local/share/agent-heavy/installation.json", home/".local/state/agent-heavy/queue.sqlite3",
                 home/".config/agent-heavy/credentials", home/"Library/LaunchAgents/io.went.agent-queue.plist"]
private_files.each do |path|
  path.dirname.mkpath
  path.write("preserved runtime sentinel: #{path.basename}")
end
witness = private_files.to_h { |path| [path, [path.binread, path.stat.ino, path.stat.mode]] }
verify_runtime = lambda do
  witness.each { |path, expected| raise "Delivery moved/deleted/changed runtime #{path}" unless path.exist? && [path.binread, path.stat.ino, path.stat.mode] == expected }
  raise "Delivery executed the app or a helper" if (home/"forbidden-execution").exist?
end
old = load_cask(fixture, 1)
old_source = stage_app(old, 1)
old.metadata_main_container_path.mkpath
old_installer = Cask::Installer.new(old, upgrade: true)
old_installer.install_artifacts
app = old.artifacts.grep(Cask::Artifact::App).first
raise "App was not relocated" unless app.source.symlink? && (app.target/"build").read == "1"
raise "App must live under the Applications directory" unless app.target == home/"Applications/Agent Queue.app"
verify_runtime.call

# A failed replacement must roll back delivery alone.
new = load_cask(fixture, 2)
stage_app(new, 2)
new.artifacts.grep(Cask::Artifact::App).first.define_singleton_method(:install_phase) { |**_options| raise "injected private app failure" }
old_installer.uninstall_artifacts(successor: new, quit: false)
raise "Old app was not backed up" unless old_source.directory? && !old_source.symlink?
verify_runtime.call
begin
  Cask::Installer.new(new, upgrade: true).install_artifacts(predecessor: old)
  raise "Delivery failure was swallowed"
rescue RuntimeError => error
  raise unless error.message == "injected private app failure"
  # Homebrew empties the app folder during an upgrade and keeps its backup.
  raise "Rollback left replacement app" if (app.target/"build").exist?
end
old_installer.install_artifacts(predecessor: new)
raise "Rollback lost old app" unless (app.target/"build").read == "1"
verify_runtime.call

new = load_cask(fixture, 2)
stage_app(new, 2)
new_installer = Cask::Installer.new(new, upgrade: true)
old_installer.uninstall_artifacts(successor: new, quit: false)
new_installer.install_artifacts(predecessor: old)
raise "Delivery upgrade did not replace the app" unless (app.target/"build").read == "2"
verify_runtime.call
new_installer.uninstall_artifacts(quit: false)
raise "Uninstall left app delivery" if app.target.exist?
verify_runtime.call
raise "Delivery changed shared permissions" unless [caskroom.parent, caskroom].map { |path| path.stat.mode & 0o777 } == shared_modes
puts "app delivery, rollback, runtime preservation and uninstall verified"
`
