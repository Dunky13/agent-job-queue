# frozen_string_literal: true

# Load the real Homebrew DSL. Optional fixture mode executes its Installer
# artifact and moves its app with a temporary HOME and shared Caskroom.
# --quarantined verifies data only. --quarantined-runtime requires a notarized
# Developer ID payload. Native lifecycle stays in the parent-owned private
# adapter; no fixture may register real Service Management jobs.
require "json"
require "find"
require "cask/cask_loader"
require "cask/caskroom"

# Homebrew starts SystemCommand children in separate process groups. Keep the
# private fixture's groups bounded even when an installer leaves descendants.
class FixtureSystemCommand < SystemCommand
  class << self
    attr_accessor :fixture, :deadline, :verify_vendor_signatures, :reject_vendor_signatures

    def configure(fixture, seconds: 60)
      self.fixture = fixture
      self.deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + seconds
      self.verify_vendor_signatures = true
      self.reject_vendor_signatures = false
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

      # Synthetic PR fixtures exercise the exact shell/copy/controller path with
      # an injected trust adapter. Release fixtures execute the real verifiers.
      if executable.to_s == "/bin/sh" && options.fetch(:args, [])[1] == unattended_installer_script && !verify_vendor_signatures
        args = options.fetch(:args).dup
        args[1] = args[1].gsub("/usr/bin/codesign", reject_vendor_signatures ? "/usr/bin/false" : "/usr/bin/true").gsub("/usr/sbin/spctl", "/usr/bin/true")
        args[1] = args[1].sub("trap 'exit 1' HUP INT TERM", "trap 'exit 1' HUP INT TERM\necho \"$work\" >> \"$HOME/vendor-staging\"")
        unless ENV["AGENT_QUEUE_INSTALL_TEST_MANAGED"] == "private-native-lifecycle"
          # A quarantined unsigned shell sentinel is data, not trusted native
          # release code. Interpret only this synthetic controller through the
          # fixture adapter; preserve every quarantine attribute on disk.
          args[1] = args[1].sub('"$payload/source/bin/agent-heavy" install', '/bin/sh "$payload/source/bin/agent-heavy" install')
        end
        options = options.merge(args: args)
      end
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

# Keep the documented vendor installer artifact distinct from sandboxed hooks.
def verify_delivery_cask_contract(cask)
  installer = cask.staged_path/"Agent Job Queue Installer.app"
  applications = cask.artifacts.grep(Cask::Artifact::App)
  target = Pathname(Dir.home)/"Applications/Agent Job Queue Installer.app"
  unless applications.length == 1 && applications.first.source == installer && applications.first.target == target
    raise "The cask must deliver only the explicit installer app to this user's Applications directory"
  end
  allowed = [Cask::Artifact::App, Cask::Artifact::Binary, Cask::Artifact::Installer, Cask::Artifact::Uninstall]
  unless cask.artifacts.all? { |artifact| allowed.include?(artifact.class) }
    raise "The cask cannot contain flight-step hooks or other artifacts"
  end
  installers = cask.artifacts.grep(Cask::Artifact::Installer)
  raise "Exactly one automatic installer artifact is required" unless installers.length == 1

  setup = installers.first
  script = setup.args.fetch(:args)
  unless setup.path == Pathname("/bin/sh") && setup.args.slice(:sudo, :must_succeed, :print_stdout) == { sudo: false, must_succeed: true, print_stdout: true } &&
         setup.args.keys.sort == %i[args must_succeed print_stdout sudo].sort &&
         script.length == 6 && script[0] == "-c" && script[2] == "agent-job-queue-install" && script[3] == cask.staged_path.to_s && script[4] == cask.version.csv.first && script[5] == cask.version.csv.second
    raise "Automatic installation must use the exact protected noninteractive vendor command"
  end
  expected_script = unattended_installer_script
  raise "Automatic installer script differs from the trusted contract" unless script[1] == expected_script

  removals = cask.artifacts.grep(Cask::Artifact::Uninstall)
  unless removals.length == 1 && removals.first.directives == { script: { executable: "/usr/bin/true", sudo: false, must_succeed: true }, signal: [] }
    raise "Homebrew removal must retain the independent runtime without lifecycle changes"
  end
  binaries = cask.artifacts.grep(Cask::Artifact::Binary)
  source = installer/"Contents/Resources/payload/source/links"
  unless binaries.map(&:source).sort == %w[agent-heavy agent-remote].map { |name| source/name }.sort &&
         binaries.all? { |binary| binary.target == cask.config.binarydir/binary.source.basename }
    raise "Both canonical CLI links are required"
  end
  artifacts = cask.artifacts.to_a
  unless artifacts.index(setup) < artifacts.index(applications.first) && binaries.all? { |binary| artifacts.index(applications.first) < artifacts.index(binary) }
    raise "Installer delivery must precede its CLI links"
  end
end

def unattended_installer_script
  <<~'SCRIPT'
    set -eu
    work=$(/usr/bin/mktemp -d /private/tmp/agent-queue-install.XXXXXXXX)
    cleanup() {
      result=$?
      trap - EXIT
      /bin/rm -rf -- "$work" || exit 1
      exit "$result"
    }
    trap cleanup EXIT
    trap 'exit 1' HUP INT TERM
    installer="$work/Agent Job Queue Installer.app"
    /usr/bin/ditto --rsrc --extattr --qtn --acl "$1/Agent Job Queue Installer.app" "$installer"
    payload="$installer/Contents/Resources/payload"
    requirement='=anchor apple generic and certificate leaf[subject.OU] = "U9GXK89565" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and notarized'
    /usr/bin/codesign --verify --strict --deep --all-architectures --check-notarization --test-requirement "$requirement and identifier \"io.went.agent-queue.installer\"" "$installer"
    /usr/bin/codesign --verify --strict --deep --all-architectures --check-notarization --test-requirement "$requirement and identifier \"io.went.agent-queue\"" "$payload/Agent Queue.app"
    /usr/bin/codesign --verify --strict --all-architectures --check-notarization --test-requirement "$requirement" "$payload/source/bin/agent-heavy"
    /usr/bin/codesign --verify --strict --all-architectures --check-notarization --test-requirement "$requirement" "$payload/source/bin/agent-remote"
    /usr/sbin/spctl --assess --type execute "$installer"
    for bundle in "$installer" "$payload/Agent Queue.app"; do
      [ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$bundle/Contents/Info.plist")" = "$2" ] &&
        [ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleVersion' "$bundle/Contents/Info.plist")" = "$3" ] || {
          echo 'The copied installer differs from the requested Homebrew release.' >&2
          exit 1
        }
    done
    "$payload/source/bin/agent-heavy" install --payload "$payload" --repair-and-continue
  SCRIPT
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

def app_helpers(app)
  info = IO.popen(["/usr/bin/plutil", "-convert", "json", "-o", "-", (app/"Contents/Info.plist").to_s], &:read)
  raise "Cannot read app metadata" unless $?.success?

  metadata = JSON.parse(info)
  if metadata.key?("AgentQueueServiceManagement")
    raise "Invalid managed marker" unless metadata.fetch("AgentQueueServiceManagement") == true

    base = app/"Contents/MacOS"
  elsif (app/"Contents/Resources/payload").exist?
    base = app/"Contents/Resources/payload/source/bin"
  else
    return []
  end
  %w[agent-heavy agent-remote].map { |name| base/name }
end

def verify_runtime_installer(installer, team, command)
  payload = installer/"Contents/Resources/payload"
  requirement = "=anchor apple generic and certificate leaf[subject.OU] = \"#{team}\" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and notarized"
  app = payload/"Agent Queue.app"
  codes = [installer, payload/"source/bin/agent-heavy", payload/"source/bin/agent-remote", app]
  codes.each do |code|
    command.run!("/usr/bin/codesign", args: ["--verify", "--strict", "--all-architectures", "--check-notarization", "--test-requirement", requirement, code])
  end
  app_helpers(app).each do |code|
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
  expected_keys << "AGENT_QUEUE_INSTALL_TEST_MANAGED" if environment.key?("AGENT_QUEUE_INSTALL_TEST_MANAGED")
  if environment.key?("AGENT_QUEUE_INSTALL_TEST_MANAGED") && environment.fetch("AGENT_QUEUE_INSTALL_TEST_MANAGED") != "private-native-lifecycle"
    raise "Invalid private native lifecycle adapter"
  end
  raise "Unexpected fixture environment fields" unless environment.keys.sort == expected_keys.sort
  raise "Fixture HOME was not preserved" unless Pathname(environment.fetch("HOME")).realpath == fixture/"User With Spaces"
  raise "Fixture tools must be private" unless Pathname(environment.fetch("AGENT_QUEUE_INSTALL_TEST_TOOLS")).realpath == fixture/"tools"
  raise "Fixture mode is required" unless environment.fetch("AGENT_QUEUE_INSTALL_TEST_MODE") == "1"

  FixtureSystemCommand.configure(fixture)
  FixtureSystemCommand.verify_vendor_signatures = runtime_fixture
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
application = Pathname(Dir.home)/"Applications/Agent Job Queue Installer.app"
verify_delivery_cask_contract(cask)

binaries = cask.artifacts.select { |artifact| artifact.is_a?(Cask::Artifact::Binary) }
raise "Both CLI binaries are required" unless binaries.map { |artifact| artifact.source.basename.to_s }.sort == %w[agent-heavy agent-remote]

artifacts = cask.artifacts.to_a

fixture_attributes = nil
fixture_app_installed = false
fixture_installer_executed = false
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
    FileUtils.cp_r((fixture/"Agent Job Queue Installer.app").children, cask.staged_path/"Agent Job Queue Installer.app")
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
  cask.artifacts.grep(Cask::Artifact::Installer).first.install_phase(command: fixture_command)
  fixture_installer_executed = true
  applications.first.install_phase
  verify_runtime_installer(application, team, fixture_command) if runtime_fixture
  fixture_app_installed = true
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
                     fixture_installer_executed: fixture_installer_executed })
