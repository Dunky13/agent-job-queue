# frozen_string_literal: true

# Load the real Homebrew DSL. Optional fixture mode moves its app and verifies
# its bytes with a temporary HOME and shared Caskroom supplied by tests.
# --quarantined verifies data only. --quarantined-runtime requires a notarized
# Developer ID payload and installs its app into a private HOME. Native first-
# launch setup acceptance runs separately after the Homebrew app installation.
require "json"
require "find"
require "cask/cask_loader"
require "cask/caskroom"

# Homebrew starts SystemCommand children in separate process groups. Keep the
# private fixture's groups bounded even when an installer leaves descendants.
class FixtureSystemCommand < SystemCommand
  class << self
    attr_accessor :fixture, :deadline

    def configure(fixture, seconds: 60)
      self.fixture = fixture
      self.deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + seconds
    end

    def record(safe, pid = nil)
      File.open(fixture/"command-groups.json", "w", 0o600) do |file|
        file.write(JSON.generate({ safe: safe, pid: pid }))
        file.flush
        file.fsync
      end
    end

    def run!(executable, **options)
      remaining = deadline - Process.clock_gettime(Process::CLOCK_MONOTONIC)
      raise Timeout::Error, "Private cask command deadline exceeded" unless remaining.positive?

      super(executable, **options.merge(timeout: [options.fetch(:timeout, remaining), remaining].min))
    end
  end

  def run!
    super
  ensure
    if @fixture_wait_thread
      pid = @fixture_wait_thread.pid
      alive = lambda do
        Process.kill(0, -pid)
        true
      rescue Errno::ESRCH, Errno::EPERM
        false
      end
      signal = lambda do |name|
        Process.kill(name, -pid)
      rescue Errno::ESRCH, Errno::EPERM
        nil
      end
      signal.call("TERM") if alive.call
      stop = Process.clock_gettime(Process::CLOCK_MONOTONIC) + 0.2
      sleep 0.01 while alive.call && Process.clock_gettime(Process::CLOCK_MONOTONIC) < stop
      signal.call("KILL") if alive.call
      stop = Process.clock_gettime(Process::CLOCK_MONOTONIC) + 2
      @fixture_wait_thread.join([stop - Process.clock_gettime(Process::CLOCK_MONOTONIC), 0].max)
      sleep 0.01 while alive.call && Process.clock_gettime(Process::CLOCK_MONOTONIC) < stop
      raise "Private installer group #{pid} survived cleanup; retain fixture tools" if alive.call

      self.class.record(true)
    end
  end

  private

  def exec3(*args, **options)
    raise "Private cask commands require their own non-root process group" unless options[:pgroup] == true

    # An interruption between spawn and PID capture must leave an unsafe record.
    self.class.record(false)
    result = super
    @fixture_wait_thread = result.last
    self.class.record(false, @fixture_wait_thread.pid)
    result
  end
end

# Allow the timeout regression to use this exact command owner without running
# the cask loader or evaluating a synthetic release as a notarized artifact.
return if __FILE__ != $PROGRAM_NAME

unless ARGV.length.between?(1, 3) && (ARGV[2].nil? || %w[--quarantined --quarantined-runtime].include?(ARGV[2]))
  raise "Expected a cask path, optional private fixture and optional --quarantined or --quarantined-runtime"
end

runtime_fixture = ARGV[2] == "--quarantined-runtime"
fixture_command = SystemCommand
raise "Runtime mode requires a private fixture" if runtime_fixture && !ARGV[1]

def verify_runtime_installer(installer, team, command)
  payload = installer/"Contents/Resources/payload"
  requirement = "=anchor apple generic and certificate leaf[subject.OU] = \"#{team}\" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and notarized"
  [installer, payload/"source/bin/agent-heavy", payload/"source/bin/agent-remote", payload/"Agent Queue.app",
   payload/"Agent Queue.app/Contents/Resources/payload/source/bin/agent-heavy",
   payload/"Agent Queue.app/Contents/Resources/payload/source/bin/agent-remote"].each do |code|
    command.run!("/usr/bin/codesign", args: ["--verify", "--strict", "--all-architectures", "--check-notarization", "--test-requirement", requirement, code])
  end
rescue ErrorDuringExecution => e
  raise "Runtime fixture failed Developer ID/notarization verification: #{e.message}"
end

cask_path = Pathname(ARGV.fetch(0))
if ARGV[1]
  fixture = Pathname(ARGV.fetch(1)).realpath
  info = fixture.stat
  unless fixture.basename.to_s.start_with?("agent-queue-install-test-") && info.uid == Process.uid && (info.mode & 0o077).zero?
    raise "Fixture must be an owned private installer-test directory"
  end
  # brew intentionally filters non-Homebrew environment variables before Ruby starts.
  # Rehydrate only this explicit fixture's bounded public test values, never host secrets.
  environment_path = fixture/"fixture-environment.json"
  raise "Unsafe fixture environment" unless (environment_path.stat.mode & 0o077).zero?

  environment = JSON.parse(environment_path.read)
  expected_keys = %w[HOME AGENT_QUEUE_INSTALL_TEST_MODE AGENT_QUEUE_INSTALL_TEST_TOOLS TEST_PYTHON_PREFIX TEST_CALLS TEST_PYTHON]
  expected_keys << "AGENT_QUEUE_INSTALL_TEST_TEAM" if runtime_fixture
  raise "Unexpected fixture environment fields" unless environment.keys.sort == expected_keys.sort
  raise "Fixture HOME was not preserved" unless Pathname(environment.fetch("HOME")).realpath == fixture/"User With Spaces"
  raise "Fixture tools must be private" unless Pathname(environment.fetch("AGENT_QUEUE_INSTALL_TEST_TOOLS")).realpath == fixture/"tools"
  raise "Fixture mode is required" unless environment.fetch("AGENT_QUEUE_INSTALL_TEST_MODE") == "1"

  FixtureSystemCommand.configure(fixture)
  fixture_command = FixtureSystemCommand
  if runtime_fixture
    team = environment.fetch("AGENT_QUEUE_INSTALL_TEST_TEAM")
    raise "Invalid expected Apple team" unless team.match?(/\A[A-Z0-9]{10}\z/)

    verify_runtime_installer(fixture/"Agent Job Queue Installer.app", team, fixture_command)
    ENV.keys.grep(/\A(?:AGENT_HEAVY_|PUEUE_)/).each { |key| ENV.delete(key) }
  end
  ENV.update(environment)

  Cask::Caskroom.instance_variable_set(:@path, fixture/"Caskroom")
end
cask = Cask::CaskLoader.load(cask_path)
payload = cask.staged_path/"Agent Job Queue Installer.app/Contents/Resources/payload"
applications = cask.artifacts.grep(Cask::Artifact::App)
application = Pathname(Dir.home)/"Applications/Agent Queue.app"
unless applications.length == 1 && applications.first.source == payload/"Agent Queue.app" && applications.first.target == application
  raise "The cask must install one Homebrew-managed app in this user's Applications directory"
end
hook_classes = [Cask::Artifact::Installer, Cask::Artifact::PreflightBlock, Cask::Artifact::PostflightBlock]
hook_classes << Cask::Artifact::PreflightSteps if defined?(Cask::Artifact::PreflightSteps)
hook_classes << Cask::Artifact::PostflightSteps if defined?(Cask::Artifact::PostflightSteps)
raise "Setup belongs to native first launch; executable install hooks are forbidden" if cask.artifacts.any? { |artifact| hook_classes.any? { |klass| artifact.is_a?(klass) } }

binaries = cask.artifacts.select { |artifact| artifact.is_a?(Cask::Artifact::Binary) }
raise "Both CLI binaries are required" unless binaries.map { |artifact| artifact.source.basename.to_s }.sort == %w[agent-heavy agent-remote]

artifacts = cask.artifacts.to_a

uninstalls = cask.artifacts.grep(Cask::Artifact::Uninstall)
unless uninstalls.length == 1 && uninstalls.first.to_h == { quit: "io.went.agent-queue", script: { executable: "/bin/rm", args: ["-f", "--", "#{Dir.home}/Library/LaunchAgents/io.went.agent-queue.plist"], sudo: false }, signal: [] }
  raise "Only the app launch agent may be deleted; Homebrew must own app removal and rollback"
end
raise "Live state must never be zapped" if cask.artifacts.any? { |artifact| artifact.is_a?(Cask::Artifact::Zap) }

fixture_attributes = nil
fixture_app_installed = false
if ARGV[1]
  raise "Fixture install requires explicit test mode" unless ENV.fetch("AGENT_QUEUE_INSTALL_TEST_MODE") == "1"

  FileUtils.mkdir_p(Cask::Caskroom.path)
  FileUtils.chmod(0o775, Cask::Caskroom.path)
  FileUtils.mkdir_p(cask.staged_path/"Agent Job Queue Installer.app/Contents/Resources")
  if runtime_fixture
    # Gatekeeper assesses the enclosing app before its nested Go executable
    # starts. Retain the actual signed installer, including its stapled ticket.
    fixture_command.run!("/usr/bin/ditto", args: ["--rsrc", "--extattr", "--qtn", "--acl", fixture/"Agent Job Queue Installer.app", cask.staged_path/"Agent Job Queue Installer.app"])
  else
    FileUtils.cp_r(fixture/"payload", payload)
  end
  if %w[--quarantined --quarantined-runtime].include?(ARGV[2])
    entries = Find.find(cask.staged_path.to_s).to_a
    raise "Quarantine fixture entries must not be symlinks" if entries.any? { |entry| File.symlink?(entry) }

    fixture_command.run!("/usr/bin/xattr", args: ["-w", "com.apple.quarantine", "0081;00000000;Test;", *entries])
    fixture_command.run!("/usr/bin/xattr", args: ["-w", "io.went.agent-queue-test", "preserved-fixture-attribute", *entries])
    before = fixture_command.run!("/usr/bin/xattr", args: ["-lr", cask.staged_path]).stdout
    fixture_attributes = {
      entries: entries.length,
      quarantine_before: before.scan(": com.apple.quarantine:").length,
      retained_before: before.scan(": io.went.agent-queue-test: preserved-fixture-attribute").length,
    }
  end
  # Recheck the actual quarantined staged bytes before native app relocation.
  verify_runtime_installer(cask.staged_path/"Agent Job Queue Installer.app", team, fixture_command) if runtime_fixture
  if !fixture_attributes || runtime_fixture
    applications.first.install_phase(command: fixture_command)
    if runtime_fixture
      requirement = "=anchor apple generic and certificate leaf[subject.OU] = \"#{team}\" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and notarized"
      [application, application/"Contents/Resources/payload/source/bin/agent-heavy", application/"Contents/Resources/payload/source/bin/agent-remote"].each do |code|
        fixture_command.run!("/usr/bin/codesign", args: ["--verify", "--strict", "--all-architectures", "--check-notarization", "--test-requirement", requirement, code])
      end
    end
    fixture_app_installed = true
  end
  if fixture_attributes
    # App relocation leaves a staging symlink. Inspect that link itself, then
    # the installed app separately, so no bundle entry is followed or counted twice.
    after = fixture_command.run!("/usr/bin/xattr", args: ["-lrs", cask.staged_path]).stdout
    after += fixture_command.run!("/usr/bin/xattr", args: ["-lr", application]).stdout if fixture_app_installed
    fixture_attributes[:quarantine_after] = after.scan(": com.apple.quarantine:").length
    fixture_attributes[:retained_after] = after.scan(": io.went.agent-queue-test: preserved-fixture-attribute").length
    unless fixture_attributes.values.all? { |count| count == fixture_attributes.fetch(:entries) }
      raise "Staged fixture must retain quarantine and other extended attributes on every staged entry"
    end
  end
end
puts JSON.generate({ version: cask.version.to_s, sha256: cask.sha256.to_s, url: cask.url.to_s,
                     dependencies: cask.depends_on.to_h,
                     app: { source: applications.first.source.to_s, target: application.to_s },
                     binaries: binaries.map { |artifact| artifact.source.to_s },
                     artifact_order: artifacts.map { |artifact| artifact.class.name },
                     fixture_attributes: fixture_attributes,
                     fixture_app_installed: fixture_app_installed,
                     fixture_installer_executed: false })
