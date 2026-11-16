import base64
import json
import os
import re
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

API = "https://api.github.com"
REPOS = "/workspace/repos"
LOG_TAIL = 4000


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


def _api_error(e):
    detail = e.read().decode(errors="replace")[:1000]
    return f"Error: GitHub API returned {e.code}: {detail}"


def _git(cwd, *args):
    # The token goes in through git's environment config, as in CloneRepo, so
    # it is neither on the command line nor saved in .git/config.
    auth = base64.b64encode(f"x-access-token:{os.environ['GITHUB_TOKEN']}".encode()).decode()
    env = dict(os.environ, GIT_TERMINAL_PROMPT="0", GIT_CONFIG_COUNT="1",
               GIT_CONFIG_KEY_0="http.https://github.com/.extraheader",
               GIT_CONFIG_VALUE_0=f"Authorization: Basic {auth}")
    cmd = ["git", "-c", "user.name=trefoil", "-c", "user.email=trefoil@users.noreply.github.com", *args]
    return subprocess.run(cmd, cwd=cwd, env=env, capture_output=True, text=True, timeout=300)


def PushBranch(args):
    try:
        repo = _repo(args.repo)
        default = _api("GET", f"/repos/{repo}")["default_branch"]
    except ValueError as e:
        return f"Error: {e}"
    except urllib.error.HTTPError as e:
        return _api_error(e)
    branch = args.branch.strip()
    if branch == default:
        return f"Error: refusing to push to the default branch {default!r}; use a feature branch"
    if _git(None, "check-ref-format", "--branch", branch).returncode != 0:
        return f"Error: {branch!r} is not a valid branch name"
    path = f"{REPOS}/{repo}"
    if not os.path.isdir(os.path.join(path, ".git")):
        return f"Error: {path} is not a clone; run CloneRepo first"

    for step in (["checkout", "-B", branch], ["add", "--all"]):
        result = _git(path, *step)
        if result.returncode != 0:
            return f"Error: git {step[0]} failed:\n{result.stderr[-2000:]}"
    if _git(path, "diff", "--cached", "--quiet").returncode != 0:
        result = _git(path, "commit", "-m", args.message)
        if result.returncode != 0:
            return f"Error: git commit failed:\n{(result.stdout + result.stderr)[-2000:]}"
    result = _git(path, "push", "origin", f"HEAD:refs/heads/{branch}")
    if result.returncode != 0:
        return f"Error: git push failed:\n{result.stderr[-2000:]}"
    sha = _git(path, "rev-parse", "HEAD").stdout.strip()
    return f"Pushed {sha[:12]} to {repo}:{branch}"


def OpenPullRequest(args):
    try:
        repo = _repo(args.repo)
        info = _api("GET", f"/repos/{repo}")
        owner = repo.split("/")[0]
        head = urllib.parse.quote(f"{owner}:{args.branch}")
        existing = _api("GET", f"/repos/{repo}/pulls?state=open&head={head}")
        if existing:
            pr, note = existing[0], "Already open"
        else:
            pr = _api("POST", f"/repos/{repo}/pulls", {
                "title": args.title,
                "body": args.body,
                "head": args.branch,
                "base": getattr(args, "base", None) or info["default_branch"],
            })
            note = "Opened"
        try:
            _api("POST", f"/repos/{repo}/labels", {"name": args.label, "color": "0e8a16"})
        except urllib.error.HTTPError as e:
            if e.code != 422:  # 422: the label exists already
                raise
        _api("POST", f"/repos/{repo}/issues/{pr['number']}/labels", {"labels": [args.label]})
    except ValueError as e:
        return f"Error: {e}"
    except urllib.error.HTTPError as e:
        return _api_error(e)
    return f"{note} pull request #{pr['number']} labelled {args.label!r}: {pr['html_url']}"


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def _job_log(repo, job_id):
    # The logs endpoint redirects to a signed URL, which must be fetched
    # without the token.
    req = urllib.request.Request(f"{API}/repos/{repo}/actions/jobs/{job_id}/logs")
    req.add_header("Authorization", f"Bearer {os.environ['GITHUB_TOKEN']}")
    try:
        urllib.request.build_opener(_NoRedirect).open(req, timeout=60)
        return ""
    except urllib.error.HTTPError as e:
        if e.code not in (301, 302, 303, 307, 308):
            return f"(log unavailable: {e.code})"
        location = e.headers["Location"]
    with urllib.request.urlopen(location, timeout=120) as resp:
        log = resp.read().decode(errors="replace")
    # Drop the timestamp at the start of each line.
    log = re.sub(r"(?m)^\d{4}-\d\d-\d\dT[\d:.]+Z ", "", log)
    return log[-LOG_TAIL:]


def _checks(repo, sha):
    runs = _api("GET", f"/repos/{repo}/commits/{sha}/check-runs?per_page=100")["check_runs"]
    statuses = _api("GET", f"/repos/{repo}/commits/{sha}/status")["statuses"]
    return runs, statuses


def WaitForChecks(args):
    try:
        repo = _repo(args.repo)
    except ValueError as e:
        return f"Error: {e}"
    timeout = (getattr(args, "timeout_minutes", None) or 20) * 60
    start = time.monotonic()
    try:
        while True:
            pr = _api("GET", f"/repos/{repo}/pulls/{args.number}")
            sha = pr["head"]["sha"]
            runs, statuses = _checks(repo, sha)
            pending = [r["name"] for r in runs if r["status"] != "completed"]
            pending += [s["context"] for s in statuses if s["state"] == "pending"]
            elapsed = time.monotonic() - start
            # Checks can take a minute to show up after a push.
            if (runs or statuses or elapsed > 120) and not pending:
                break
            if elapsed > timeout:
                return (f"Timed out after {timeout // 60} min on {sha[:12]}; still running: "
                        + ", ".join(pending or ["(no checks reported yet)"]))
            time.sleep(20)

        if not runs and not statuses:
            return f"No checks reported on {sha[:12]} after 2 min; the repo may have no CI for this branch."

        lines, failed = [f"Checks on {sha[:12]} ({pr['html_url']}):"], []
        for r in runs:
            lines.append(f"- {r['name']}: {r['conclusion']}")
            if r["conclusion"] in ("failure", "timed_out", "cancelled", "action_required"):
                failed.append(r)
        for s in statuses:
            lines.append(f"- {s['context']}: {s['state']} {s.get('description') or ''}".rstrip())
        for r in failed:
            lines.append(f"\n## {r['name']} ({r['html_url']})")
            output = r.get("output") or {}
            if output.get("summary"):
                lines.append(output["summary"][:1000])
            if (r.get("app") or {}).get("slug") == "github-actions":
                lines.append("Log tail:\n" + _job_log(repo, r["id"]))
        bad = len(failed) + sum(s["state"] in ("failure", "error") for s in statuses)
        lines.insert(1, "All checks passed." if not bad else f"{bad} check(s) did not pass.")
        return "\n".join(lines)
    except urllib.error.HTTPError as e:
        return _api_error(e)
