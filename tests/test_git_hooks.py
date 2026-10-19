"""Exercise the real pre-push hook without creating or changing Git commits."""
from __future__ import annotations

import os
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
ZERO = "0" * 40
HEAD = "1" * 40
MAIN = "2" * 40
REMOTE = "3" * 40


class PrePushTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="agent-queue-git-hook-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.tools = self.root / "tools"
        self.tools.mkdir()
        self.calls = self.root / "calls.tsv"
        self.environment = {**os.environ, "PATH": str(self.tools) + os.pathsep + os.environ.get("PATH", ""),
                            "HOOK_CALLS": str(self.calls), "HOOK_MAIN": MAIN}
        # Use shell stubs so the tested hook remains independent of Python/Git
        # configuration, object stores and signing keys on the host.
        self.tool("git", '''printf 'git' >> "$HOOK_CALLS"
for argument; do printf '\\t%s' "$argument" >> "$HOOK_CALLS"; done
printf '\\n' >> "$HOOK_CALLS"
case "$1" in
  show-ref) exit "${HOOK_LOOKUP_STATUS:-0}" ;;
  rev-parse)
    printf '%s\\n' "$HOOK_MAIN"
    exit "${HOOK_RESOLVE_STATUS:-0}"
    ;;
  *) exit 99 ;;
esac
''')
        self.tool("python3", '''printf 'python3' >> "$HOOK_CALLS"
for argument; do printf '\\t%s' "$argument" >> "$HOOK_CALLS"; done
printf '\\n' >> "$HOOK_CALLS"
exit "${HOOK_CHECK_STATUS:-0}"
''')

    def tool(self, name, body):
        path = self.tools / name
        path.write_text("#!/bin/sh\nset -eu\n" + body)
        path.chmod(0o755)

    def push(self, *, remote=REMOTE, local=HEAD, remote_ref="refs/heads/feature", environment=None):
        result = subprocess.run(["/bin/sh", str(ROOT / ".githooks/pre-push"), "origin", "unused-remote"],
                                input=f"refs/heads/feature {local} {remote_ref} {remote}\n", cwd=self.root,
                                env={**self.environment, **(environment or {})}, capture_output=True,
                                text=True, timeout=5, check=False)
        calls = [line.split("\t") for line in self.calls.read_text().splitlines()] if self.calls.exists() else []
        return result, calls

    def assert_checker(self, calls, base):
        self.assertIn(["python3", "scripts/check-commits.py", "--base", base, "--head", HEAD, "--verify-local"], calls)

    def test_new_branch_checks_only_complete_pr_range_from_origin_main(self):
        result, calls = self.push(remote=ZERO)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls[:2], [["git", "show-ref", "--verify", "--quiet", "refs/remotes/origin/main"],
                                    ["git", "rev-parse", "--verify", "refs/remotes/origin/main^{commit}"]])
        self.assert_checker(calls, MAIN)

    def test_existing_branch_still_checks_previously_pushed_pr_commits(self):
        result, calls = self.push(remote=REMOTE)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_checker(calls, MAIN)
        self.assertNotIn(["--base", REMOTE], [call[2:4] for call in calls if call[0] == "python3"])

    def test_bootstrap_without_origin_main_uses_remote_or_initial_history(self):
        for remote in (ZERO, REMOTE):
            with self.subTest(remote=remote):
                self.calls.unlink(missing_ok=True)
                result, calls = self.push(remote=remote, environment={"HOOK_LOOKUP_STATUS": "1"})
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assert_checker(calls, remote)
                self.assertFalse(any(call[1] == "rev-parse" for call in calls if call[0] == "git"))

    def test_unexpected_main_lookup_failure_blocks_without_running_checker(self):
        result, calls = self.push(environment={"HOOK_LOOKUP_STATUS": "128"})
        self.assertEqual(result.returncode, 128)
        self.assertIn("push blocked", result.stderr)
        self.assertFalse(any(call[0] == "python3" for call in calls))

    def test_unresolvable_main_commit_blocks_instead_of_falling_back(self):
        result, calls = self.push(environment={"HOOK_RESOLVE_STATUS": "128"})
        self.assertEqual(result.returncode, 128)
        self.assertFalse(any(call[0] == "python3" for call in calls))

    def test_signature_or_dco_checker_failure_blocks(self):
        result, calls = self.push(environment={"HOOK_CHECK_STATUS": "17"})
        self.assertEqual(result.returncode, 17)
        self.assert_checker(calls, MAIN)

    def test_deletion_and_nonbranch_refs_do_not_run_checks(self):
        for local, reference in ((ZERO, "refs/heads/feature"), (HEAD, "refs/tags/v1")):
            with self.subTest(local=local, reference=reference):
                self.calls.unlink(missing_ok=True)
                result, calls = self.push(local=local, remote_ref=reference)
                self.assertEqual(result.returncode, 0)
                self.assertEqual(calls, [])


if __name__ == "__main__":
    unittest.main()
