#!/usr/bin/env python3
"""Require matching DCO trailers and optionally verify Git signatures."""
import argparse
import json
import subprocess
import sys

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--base", default="")
parser.add_argument("--head", default="HEAD")
parser.add_argument("--verify-local", action="store_true")
parser.add_argument("--github-repository")
args = parser.parse_args()


def git(*arguments):
    return subprocess.check_output(["git", *arguments], text=True).strip()


revision = f"{args.base}..{args.head}" if args.base and set(args.base) != {"0"} else args.head
commits = git("rev-list", revision).splitlines()
for commit in commits:
    author, email, body = git("show", "-s", "--format=%an%n%ae%n%B", commit).split("\n", 2)
    required = f"Signed-off-by: {author} <{email}>"
    if required not in body.splitlines():
        sys.exit(f"{commit}: missing author-matching DCO sign-off")
    if args.verify_local:
        subprocess.run(["git", "verify-commit", commit], check=True)
    if args.github_repository:
        response = subprocess.check_output(
            ["gh", "api", f"repos/{args.github_repository}/commits/{commit}"], text=True
        )
        if not json.loads(response)["commit"]["verification"]["verified"]:
            sys.exit(f"{commit}: GitHub does not report a verified cryptographic signature")
print(f"Commit policy passed ({len(commits)} commits).")
