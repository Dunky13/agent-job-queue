# frozen_string_literal: true

# Load the real Homebrew DSL and enforce the plain app cask contract: the only
# artifacts are the explicit app and its quit-only uninstall. Optional fixture
# mode moves the app into a temporary HOME and shared Caskroom, never into the
# host's /Applications. --quarantined verifies quarantine retention on data
# only. --quarantined-runtime TEAM requires the notarized Developer ID app.
# No fixture may run the app, register Service Management jobs or touch launchd:
# setup happens when a person opens the app, not when Homebrew installs it.
require "json"
require "find"
require "cask/cask_loader"
require "cask/caskroom"

APP_NAME = "Agent Queue.app"
APP_IDENTIFIER = "io.went.agent-queue"

def verify_delivery_cask_contract(cask)
  staged = cask.staged_path/APP_NAME
  applications = cask.artifacts.grep(Cask::Artifact::App)
  unless applications.length == 1 && applications.first.source == staged && applications.first.target == cask.config.appdir/APP_NAME
    raise "The cask must deliver only the explicit app to the Applications directory"
  end
  allowed = [Cask::Artifact::App, Cask::Artifact::Uninstall]
  unless cask.artifacts.length == allowed.length && cask.artifacts.all? { |artifact| allowed.include?(artifact.class) }
    raise "The cask cannot contain installer scripts, binary links, flight-step hooks or other artifacts"
  end

  removals = cask.artifacts.grep(Cask::Artifact::Uninstall)
  unless removals.length == 1 && removals.first.directives == { quit: APP_IDENTIFIER, signal: [] }
    raise "Homebrew removal must only ask the running app to quit"
  end
end

# Allow regressions to load the contract check without running the fixture.
return if __FILE__ != $PROGRAM_NAME

MODES = %w[--quarantined --quarantined-runtime].freeze
valid_arguments = ARGV.length.between?(1, 4) && (ARGV[2].nil? || MODES.include?(ARGV[2])) &&
                  (ARGV[2] == "--quarantined-runtime" ? ARGV.length == 4 : ARGV.length <= 3)
unless valid_arguments
  raise "Expected a cask path, an optional private fixture and an optional --quarantined or --quarantined-runtime TEAM"
end

runtime_fixture = ARGV[2] == "--quarantined-runtime"
raise "Fixture modes require a private fixture" if ARGV[2] && !ARGV[1]

def verify_runtime_app(app, team, command)
  requirement = "=anchor apple generic and certificate leaf[subject.OU] = \"#{team}\" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and notarized"
  helpers = %w[AgentQueue agent-heavy agent-remote].map { |name| app/"Contents/MacOS"/name }
  [app, *helpers].each do |code|
    suffix = code == app ? " and identifier \"#{APP_IDENTIFIER}\"" : ""
    command.run!("/usr/bin/codesign", args: ["--verify", "--strict", "--all-architectures", "--check-notarization", "--test-requirement", requirement + suffix, code])
  end
  command.run!("/usr/sbin/spctl", args: ["--assess", "--type", "execute", app])
rescue ErrorDuringExecution => e
  raise "Runtime fixture failed Developer ID/notarization verification: #{e.message}"
end

cask_path = Pathname(ARGV.fetch(0))
fixture = nil
if ARGV[1]
  fixture = Pathname(ARGV.fetch(1)).realpath
  info = fixture.stat
  unless fixture.basename.to_s.start_with?("agent-queue-cask-test-") && info.uid == Process.uid && (info.mode & 0o077).zero?
    raise "Fixture must be an owned private cask-test directory"
  end
  raise "Fixture HOME was not preserved" unless Pathname(Dir.home).realpath == fixture/"User With Spaces"

  Cask::Caskroom.instance_variable_set(:@path, fixture/"Caskroom")
end
cask = Cask::CaskLoader.load(cask_path)
application = Pathname(Dir.home)/"Applications"/APP_NAME
cask.config = Cask::Config.new(explicit: { appdir: application.dirname.to_s }) if fixture
verify_delivery_cask_contract(cask)
applications = cask.artifacts.grep(Cask::Artifact::App)

fixture_attributes = nil
fixture_app_installed = false
if fixture
  team = ARGV[3]
  raise "Invalid expected Apple team" if runtime_fixture && !team.to_s.match?(/\A[A-Z0-9]{10}\z/)

  # Reject untrusted bytes before creating any Caskroom or Applications state.
  verify_runtime_app(fixture/APP_NAME, team, SystemCommand) if runtime_fixture
  FileUtils.mkdir_p(Cask::Caskroom.path)
  FileUtils.chmod(0o775, Cask::Caskroom.path)
  FileUtils.mkdir_p(application.dirname)
  # Retain the actual bundle, including any stapled ticket and quarantine.
  SystemCommand.run!("/usr/bin/ditto", args: ["--rsrc", "--extattr", "--qtn", "--acl", fixture/APP_NAME, cask.staged_path/APP_NAME])
  if ARGV[2]
    entries = Find.find(cask.staged_path.to_s).to_a
    raise "Quarantine fixture entries must not be symlinks" if entries.any? { |entry| File.symlink?(entry) }

    SystemCommand.run!("/usr/bin/xattr", args: ["-w", "com.apple.quarantine", "0081;00000000;Test;", *entries])
    SystemCommand.run!("/usr/bin/xattr", args: ["-w", "io.went.agent-queue-test", "preserved-fixture-attribute", *entries])
    before = SystemCommand.run!("/usr/bin/xattr", args: ["-lr", cask.staged_path]).stdout
    fixture_attributes = {
      entries: entries.length,
      quarantine_before: before.scan(": com.apple.quarantine:").length,
      retained_before: before.scan(": io.went.agent-queue-test: preserved-fixture-attribute").length,
    }
  end
  # Recheck the actual quarantined staged bytes before native app relocation.
  verify_runtime_app(cask.staged_path/APP_NAME, team, SystemCommand) if runtime_fixture
  applications.first.install_phase
  verify_runtime_app(application, team, SystemCommand) if runtime_fixture
  fixture_app_installed = true
  if fixture_attributes
    # App relocation leaves a staging symlink. Inspect that link itself, then
    # the installed app separately, so no bundle entry is followed or counted twice.
    after = SystemCommand.run!("/usr/bin/xattr", args: ["-lrs", cask.staged_path]).stdout
    after += SystemCommand.run!("/usr/bin/xattr", args: ["-lr", application]).stdout
    fixture_attributes[:quarantine_after] = after.scan(": com.apple.quarantine:").length
    fixture_attributes[:retained_after] = after.scan(": io.went.agent-queue-test: preserved-fixture-attribute").length
    unless fixture_attributes.values.all? { |count| count == fixture_attributes.fetch(:entries) }
      raise "Staged fixture must retain quarantine and other extended attributes on every staged entry"
    end
  end
end
puts JSON.generate({ version: cask.version.to_s, sha256: cask.sha256.to_s, url: cask.url.to_s,
                     dependencies: cask.depends_on.to_h,
                     app: { source: applications.first.source.to_s, target: applications.first.target.to_s },
                     artifact_order: cask.artifacts.map { |artifact| artifact.class.name },
                     fixture_attributes: fixture_attributes,
                     fixture_app_installed: fixture_app_installed })
