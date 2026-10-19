# frozen_string_literal: true

# Load the real Homebrew DSL. Optional fixture mode executes its preflight and
# installer artifacts with a temporary HOME and Caskroom supplied by installer tests.
require "json"
require "find"
require "cask/cask_loader"
require "cask/caskroom"

unless ARGV.length.between?(1, 3) && (ARGV[2].nil? || ARGV[2] == "--quarantined")
  raise "Expected a cask path, optional private fixture and optional --quarantined"
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
  raise "Unexpected fixture environment fields" unless environment.keys.sort == expected_keys.sort
  raise "Fixture HOME was not preserved" unless Pathname(environment.fetch("HOME")).realpath == fixture/"User With Spaces"
  raise "Fixture tools must be private" unless Pathname(environment.fetch("AGENT_QUEUE_INSTALL_TEST_TOOLS")).realpath == fixture/"tools"
  raise "Fixture mode is required" unless environment.fetch("AGENT_QUEUE_INSTALL_TEST_MODE") == "1"

  ENV.update(environment)

  Cask::Caskroom.instance_variable_set(:@path, fixture/"Caskroom")
end
cask = Cask::CaskLoader.load(cask_path)
installer = cask.artifacts.find { |artifact| artifact.is_a?(Cask::Artifact::Installer) }
details = installer&.to_h
arguments = details&.fetch(:args, nil)
# Manual installers omit :args. Require the declared false value rather than
# relying on newer Homebrew methods or treating a missing privilege flag as safe.
unless arguments.is_a?(Hash) && arguments[:sudo] == false
  raise "An automated non-root installer is required"
end

binaries = cask.artifacts.select { |artifact| artifact.is_a?(Cask::Artifact::Binary) }
raise "Both CLI binaries are required" unless binaries.map { |artifact| artifact.source.basename.to_s }.sort == %w[agent-heavy agent-remote]

preflight_classes = [Cask::Artifact::PreflightBlock]
preflight_classes << Cask::Artifact::PreflightSteps if defined?(Cask::Artifact::PreflightSteps)
preflights = cask.artifacts.select { |artifact| preflight_classes.include?(artifact.class) }
raise "Exactly one preflight block is required" unless preflights.length == 1

preflight = preflights.fetch(0)
# ArtifactSet#to_a uses the installed Homebrew implementation's artifact sort order,
# which is the order Cask::Installer#install_artifacts executes.
artifacts = cask.artifacts.to_a
unless [installer, *binaries].all? { |artifact| artifacts.index(preflight) < artifacts.index(artifact) }
  raise "The preflight block must run before the installer and both CLI binaries"
end

raise "A safe uninstall is required" unless cask.artifacts.any? { |artifact| artifact.is_a?(Cask::Artifact::Uninstall) }
raise "Live state must never be zapped" if cask.artifacts.any? { |artifact| artifact.is_a?(Cask::Artifact::Zap) }

fixture_attributes = nil
if ARGV[1]
  raise "Fixture install requires explicit test mode" unless ENV.fetch("AGENT_QUEUE_INSTALL_TEST_MODE") == "1"

  FileUtils.mkdir_p(cask.staged_path/"Agent Job Queue Installer.app/Contents/Resources")
  FileUtils.cp_r(fixture/"payload", cask.staged_path/"Agent Job Queue Installer.app/Contents/Resources/payload")
  if ARGV[2] == "--quarantined"
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
  preflight.install_phase(command: SystemCommand)
  if fixture_attributes
    after = SystemCommand.run!("/usr/bin/xattr", args: ["-lr", cask.staged_path]).stdout
    fixture_attributes[:quarantine_after] = after.scan(": com.apple.quarantine:").length
    fixture_attributes[:retained_after] = after.scan(": io.went.agent-queue-test: preserved-fixture-attribute").length
  end
  installer.install_phase(command: SystemCommand)
end
puts JSON.generate({ version: cask.version.to_s, sha256: cask.sha256.to_s, url: cask.url.to_s,
                     dependencies: cask.depends_on.to_h, installer: installer.to_h,
                     binaries: binaries.map { |artifact| artifact.source.to_s },
                     artifact_order: artifacts.map { |artifact| artifact.class.name },
                     fixture_attributes: fixture_attributes })
