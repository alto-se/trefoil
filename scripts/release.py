#!/usr/bin/env python3
"""Cut a release from the VERSION file and conventional commits.

VERSION is the source of truth (git tags are only written, never read):

  - If VERSION was changed by hand since the last `chore(release):` commit,
    that exact version is released.
  - Otherwise the commits since the last release bump it: `feat` -> minor,
    `fix`/`perf` -> patch, a breaking change (`type!:` or `BREAKING CHANGE:`)
    -> major, or minor while the version is 0.x. Other types release nothing.

A release writes VERSION, cli/pyproject.toml and cli/uv.lock, commits them as
`chore(release): X [skip ci]`, tags vX, pushes both and creates a GitHub
release. `--dry-run` only prints what it would do. Outputs `published`,
`version` and `tag` to $GITHUB_OUTPUT when set.
"""

import os
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
RELEASE_PREFIX = "chore(release):"
HEADER = re.compile(r"^(\w+)(\([^)]*\))?(!)?:\s*(.+)$")


def git(*args):
    return subprocess.run(["git", *args], cwd=ROOT, check=True, capture_output=True, text=True).stdout.strip()


def parse(version):
    m = re.fullmatch(r"(\d+)\.(\d+)\.(\d+)", version)
    if not m:
        sys.exit(f"release: VERSION {version!r} is not MAJOR.MINOR.PATCH")
    return tuple(int(x) for x in m.groups())


def last_release():
    """Returns (sha, version) of the newest `chore(release): X` commit, or (None, None)."""
    for line in git("log", "--format=%H %s").splitlines():
        sha, _, subject = line.partition(" ")
        if subject.startswith(RELEASE_PREFIX):
            m = re.search(r"(\d+\.\d+\.\d+)", subject)
            return sha, m.group(1) if m else None
    return None, None


def commits_since(sha):
    rng = f"{sha}..HEAD" if sha else "HEAD"
    out = git("log", "--format=%H%x1f%s%x1f%b%x1e", rng)
    commits = []
    for entry in out.split("\x1e"):
        if not entry.strip():
            continue
        h, subject, body = (entry.strip("\n").split("\x1f") + ["", ""])[:3]
        if subject.startswith(RELEASE_PREFIX):
            continue
        commits.append((h, subject, body))
    return commits


def bump(version, commits):
    major, minor, patch = parse(version)
    level = None  # 0 patch, 1 minor, 2 major
    for _, subject, body in commits:
        m = HEADER.match(subject)
        if not m:
            continue
        kind, breaking = m.group(1), bool(m.group(3)) or "BREAKING CHANGE:" in body
        if breaking:
            level = max(level or 0, 2)
        elif kind == "feat":
            level = max(level or 0, 1)
        elif kind in ("fix", "perf"):
            level = max(level or 0, 0)
    if level is None:
        return None
    if level == 2 and major == 0:
        level = 1  # still pre-1.0: breaking changes bump minor
    if level == 2:
        return f"{major + 1}.0.0"
    if level == 1:
        return f"{major}.{minor + 1}.0"
    return f"{major}.{minor}.{patch + 1}"


def notes(version, commits):
    sections = {"feat": "Features", "fix": "Bug fixes", "perf": "Performance"}
    grouped = {k: [] for k in sections}
    breaking = []
    for h, subject, body in commits:
        m = HEADER.match(subject)
        if not m:
            continue
        line = f"- {m.group(4)} ({h[:7]})"
        if m.group(3) or "BREAKING CHANGE:" in body:
            breaking.append(line)
        if m.group(1) in grouped:
            grouped[m.group(1)].append(line)
    out = [f"## {version}"]
    if breaking:
        out += ["", "### Breaking changes", *breaking]
    for k, title in sections.items():
        if grouped[k]:
            out += ["", f"### {title}", *grouped[k]]
    return "\n".join(out) + "\n"


def write_version(version):
    (ROOT / "VERSION").write_text(version + "\n")
    for path, pattern, repl in [
        ("cli/pyproject.toml", r'(?m)^version = ".*"$', f'version = "{version}"'),
        ("cli/uv.lock", r'(name = "trefoil-cli"\nversion = )".*"', rf'\g<1>"{version}"'),
    ]:
        f = ROOT / path
        if f.exists():
            f.write_text(re.sub(pattern, repl, f.read_text(), count=1))


def output(**kv):
    path = os.environ.get("GITHUB_OUTPUT")
    for k, v in kv.items():
        print(f"{k}={v}")
        if path:
            with open(path, "a") as f:
                f.write(f"{k}={v}\n")


def main():
    dry = "--dry-run" in sys.argv[1:]
    current = (ROOT / "VERSION").read_text().strip()
    parse(current)
    sha, released = last_release()
    commits = commits_since(sha)

    if released != current:
        version = current  # set by hand since the last release
        print(f"release: VERSION set to {current} by hand (last release: {released or 'none'})", file=sys.stderr)
    else:
        version = bump(current, commits)
        if version is None:
            print(f"release: no releasable commits since {released}", file=sys.stderr)
            output(published="false")
            return
        print(f"release: {current} -> {version} ({len(commits)} commits)", file=sys.stderr)

    tag = f"v{version}"
    if git("tag", "-l", tag):
        sys.exit(f"release: tag {tag} already exists; set VERSION to the version to release")

    body = notes(version, commits)
    if dry:
        print(body, file=sys.stderr)
        output(published="false", version=version, tag=tag)
        return

    write_version(version)
    git("add", "VERSION", "cli/pyproject.toml", "cli/uv.lock")
    git("commit", "-m", f"{RELEASE_PREFIX} {version} [skip ci]", "-m", body)
    git("tag", tag)
    git("push", "origin", "HEAD", tag)
    subprocess.run(["gh", "release", "create", tag, "--title", tag, "--notes", body], cwd=ROOT, check=True)
    output(published="true", version=version, tag=tag)


if __name__ == "__main__":
    main()
