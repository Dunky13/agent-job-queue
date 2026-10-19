# frozen_string_literal: true

# Load the real Homebrew DSL. Optional fixture mode executes its installer artifact
# with a temporary HOME and Caskroom supplied by isolated installer tests.
require "json"
require "cask/cask_loader"
require "cask/caskroom"

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
raise "An automated non-root installer is required" if installer.nil? || installer.manual_install || installer.requires_sudo?

binaries = cask.artifacts.select { |artifact| artifact.is_a?(Cask::Artifact::Binary) }
raise "Both CLI binaries are required" unless binaries.map { |artifact| artifact.source.basename.to_s }.sort == %w[agent-heavy agent-remote]

raise "A safe uninstall is required" unless cask.artifacts.any? { |artifact| artifact.is_a?(Cask::Artifact::Uninstall) }
raise "Live state must never be zapped" if cask.artifacts.any? { |artifact| artifact.is_a?(Cask::Artifact::Zap) }

if ARGV[1]
  raise "Fixture install requires explicit test mode" unless ENV.fetch("AGENT_QUEUE_INSTALL_TEST_MODE") == "1"

  FileUtils.mkdir_p(cask.staged_path/"Agent Job Queue Installer.app/Contents/Resources")
  FileUtils.cp_r(fixture/"payload", cask.staged_path/"Agent Job Queue Installer.app/Contents/Resources/payload")
  installer.install_phase
end
puts JSON.generate({ version: cask.version.to_s, sha256: cask.sha256.to_s, url: cask.url.to_s,
                     dependencies: cask.depends_on.to_h, installer: installer.to_h,
                     binaries: binaries.map { |artifact| artifact.source.to_s } })
