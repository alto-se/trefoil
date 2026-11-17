import base64
import json
import os
import re
import subprocess
import urllib.error
import urllib.parse
import urllib.request

API = "https://api.github.com"


def CloneRepo(args):
    repo = args.repo.strip().removeprefix("https://github.com/").removesuffix(".git")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo):
        return f"Error: repo must look like owner/name, got {args.repo!r}"
    dest = f"/workspace/repos/{repo}"
    if os.path.isdir(dest):
        return dest

    # The token goes in through git's environment config, so it is neither
    # on the command line nor saved in the clone's .git/config.
    auth = base64.b64encode(f"x-access-token:{os.environ['GITHUB_TOKEN']}".encode()).decode()
    env = dict(os.environ, GIT_TERMINAL_PROMPT="0", GIT_CONFIG_COUNT="1",
               GIT_CONFIG_KEY_0="http.https://github.com/.extraheader",
               GIT_CONFIG_VALUE_0=f"Authorization: Basic {auth}")
    cmd = ["git", "clone", "--depth", "1"]
    if getattr(args, "ref", None):
        cmd += ["--branch", args.ref]
    cmd += [f"https://github.com/{repo}.git", dest]
    result = subprocess.run(cmd, env=env, capture_output=True, text=True, timeout=300)
    if result.returncode != 0:
        return f"Error: git clone failed:\n{result.stderr[-2000:]}"
    return dest


def _repo(value):
    repo = value.strip().removeprefix("https://github.com/").removesuffix(".git")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo):
        raise ValueError(f"repo must look like owner/name, got {value!r}")
    return repo


def _api(method, path, body=None):
    req = urllib.request.Request(API + path, method=method, data=json.dumps(body).encode() if body is not None else None)
    req.add_header("Authorization", f"Bearer {os.environ['GITHUB_TOKEN']}")
    req.add_header("Accept", "application/vnd.github+json")
    req.add_header("X-GitHub-Api-Version", "2022-11-28")
    if body is not None:
        req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=60) as resp:
        data = resp.read()
    return json.loads(data) if data else None


def _ensure_label(repo, name):
    try:
        _api("GET", f"/repos/{repo}/labels/{urllib.parse.quote(name, safe='')}")
    except urllib.error.HTTPError as e:
        if e.code != 404:
            raise
        _api("POST", f"/repos/{repo}/labels", {"name": name})


def CreateIssue(args):
    try:
        repo = _repo(args.repo)
        labels = [label.strip() for label in (getattr(args, "labels", None) or []) if label.strip()]
        for label in labels:
            _ensure_label(repo, label)
        issue = _api("POST", f"/repos/{repo}/issues", {"title": args.title, "body": args.body, "labels": labels})
    except ValueError as e:
        return f"Error: {e}"
    except urllib.error.HTTPError as e:
        return f"Error: GitHub API returned {e.code}: {e.read().decode(errors='replace')[:1000]}"
    return f"Created issue #{issue['number']}: {issue['html_url']}"
