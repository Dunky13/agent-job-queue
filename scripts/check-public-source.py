#!/usr/bin/env python3
"""Keep host data and local checkout paths out of published source."""
from pathlib import Path
import re
import subprocess
import sys

root = Path(__file__).resolve().parents[1]
paths = subprocess.check_output(
    ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"], cwd=root
).decode().split("\0")
failures = []
private_suffixes = {".log", ".sqlite", ".sqlite3", ".pem", ".key", ".p12", ".pfx"}
for name in filter(None, paths):
    path = root / name
    if path.is_symlink():
        failures.append(f"{name}: source symlinks require an explicit packaging review")
        continue
    if not path.is_file():
        continue
    if path.suffix in private_suffixes or any(part in {".venv", "node_modules", "__pycache__", ".t3"} for part in path.relative_to(root).parts):
        failures.append(f"{name}: private or generated data must not be published")
    body = path.read_bytes()
    if re.search(rb"/Users/[A-Za-z0-9_.-]+/|/home/[A-Za-z0-9_.-]+/", body):
        failures.append(f"{name}: machine-specific home or checkout path")
    if re.search(rb"-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----", body):
        failures.append(f"{name}: private key material")
if failures:
    print("\n".join(failures), file=sys.stderr)
    sys.exit(1)
print(f"Public-source boundary passed ({len(set(filter(None, paths)))} files).")
