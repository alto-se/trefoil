import base64
import os
import re
import subprocess


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
