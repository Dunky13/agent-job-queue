"""Exercise the real delivery configuration check without accessing credentials."""
from __future__ import annotations

import os
import subprocess
import textwrap
import unittest
from pathlib import Path


class DeliveryConfigurationTests(unittest.TestCase):
    def test_missing_configuration_fails_without_echoing_inputs(self):
        workflow = (Path(__file__).resolve().parents[1] / ".github/workflows/ci.yml").read_text()
        step = workflow.split("      - name: Require Homebrew dispatch configuration\n", 1)[1]
        script = textwrap.dedent(step.split("        run: |\n", 1)[1].split("      - name:", 1)[0])
        for client, key, expected in (("", "", 1), ("public-id-fixture", "", 1),
                                      ("", "key-input-fixture", 1),
                                      ("public-id-fixture", "key-input-fixture", 0)):
            with self.subTest(client_present=bool(client), key_present=bool(key)):
                result = subprocess.run(["bash", "-eu", "-o", "pipefail", "-c", script],
                                        env={**os.environ, "APP_CLIENT_ID": client, "APP_PRIVATE_KEY": key},
                                        capture_output=True, text=True, timeout=5, check=False)
                self.assertEqual(result.returncode, expected)
                self.assertNotIn("public-id-fixture", result.stdout + result.stderr)
                self.assertNotIn("key-input-fixture", result.stdout + result.stderr)
                if expected:
                    self.assertIn("::error::Configure HOMEBREW_SYNC_APP_CLIENT_ID", result.stdout)


if __name__ == "__main__":
    unittest.main()
