#!/usr/bin/env python3
"""Bundle a trefoil config.yml into a single JSON file.

Resolves:
  - `!include file://path` and `!include https://url` tags
  - bare `file://path` strings (bare https:// strings are left as-is)

Referenced .yml/.yaml/.json files are parsed and resolved recursively;
a referenced folder becomes {relative path: contents} and a .zip a
base64 data: URL (the two ways to package a local skill); anything else is
inlined as text. Relative paths resolve against the
directory of the file that references them.

Usage: trefoil <domain> <command> [args]

  trefoil init [DIR] [--example=NAME]
  trefoil update
  trefoil engine start [--settings=backend.yml] [--port=8080] [--foreground]
                       [--setup] [--provider=bedrock|anthropic] [--aws-profile=P] [--aws-region=R]
  trefoil engine stop
  trefoil config package path/to/config.yml [-o bundle.json]
  trefoil config sync [path/to/config.yml] [--server URL]
  trefoil agents list
  trefoil agents build --agent=NAME [-d] [--dockerfile]
  trefoil agents get-dockerfile --agent=NAME [-o FILE]
  trefoil agents run --agent=NAME [TASK | --input=JSON|file://PATH] [-d]
  trefoil sessions status|logs|result --agent=NAME ID
  trefoil sessions rerun --agent=NAME ID [-d]
  trefoil sessions artifacts --agent=NAME ID [-o DIR]
  trefoil secrets list
  trefoil secrets set --name=NAME
  trefoil jobs list [--agent=NAME] [--status=STATUS]
  trefoil jobs status ID [-f] [--logs]
"""

import argparse
import base64
import getpass
import importlib.metadata
import importlib.util
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request
from urllib.parse import quote, urlencode
from pathlib import Path

import yaml

DEFAULT_SERVER = "http://localhost:8080"
EXAMPLES_API = "https://api.github.com/repos/alto-se/trefoil/contents/examples"
ENGINE_REPO = "ghcr.io/alto-se/trefoil-api"


class Ref:
    def __init__(self, url):
        self.url = url


class Loader(yaml.SafeLoader):
    pass


for _tag in ("!include", "!Ref"):  # !Ref is the old name, still accepted
    Loader.add_constructor(_tag, lambda loader, node: Ref(loader.construct_scalar(node)))


class Packager:
    def __init__(self):
        self.errors = []
        self.stack = []  # files currently being resolved, for cycle detection

    def resolve(self, value, base):
        if isinstance(value, dict):
            return {k: self.resolve(v, base) for k, v in value.items()}
        if isinstance(value, list):
            return [self.resolve(v, base) for v in value]
        if isinstance(value, Ref):
            return self.load(value.url, base)
        if isinstance(value, str) and value.startswith("file://"):
            return self.load(value, base)
        return value

    def load(self, url, base):
        try:
            if url.startswith("file://"):
                path = (base / url.removeprefix("file://")).resolve()
                if path in self.stack:
                    raise ValueError(f"circular reference: {path}")
                if path.is_dir():
                    return read_folder(path)
                if path.suffix == ".zip":
                    return "data:application/zip;base64," + base64.b64encode(path.read_bytes()).decode()
                text = path.read_text()
                name, next_base, key = path.name, path.parent, path
            elif url.startswith(("http://", "https://")):
                with urllib.request.urlopen(url, timeout=30) as resp:
                    text = resp.read().decode()
                name, next_base, key = url, base, url
            else:
                raise ValueError(f"unsupported ref: {url}")
        except Exception as e:
            self.errors.append(f"{url}: {e}")
            return None

        if name.endswith((".yml", ".yaml", ".json")):
            self.stack.append(key)
            try:
                return self.resolve(yaml.load(text, Loader=Loader), next_base)
            finally:
                self.stack.pop()
        return text

    def package(self, config_path):
        config_path = config_path.resolve()
        self.stack.append(config_path)
        return self.resolve(yaml.load(config_path.read_text(), Loader=Loader), config_path.parent)


def read_folder(root):
    """Every file under root as {relative path: text}, skipping hidden files
    and __pycache__, like shared modules on the server."""
    files = {}
    for path in sorted(root.rglob("*")):
        rel = path.relative_to(root)
        if any(p.startswith(".") or p == "__pycache__" for p in rel.parts) or not path.is_file():
            continue
        try:
            files[rel.as_posix()] = path.read_text()
        except UnicodeDecodeError:
            raise ValueError(f"{rel}: only text files are supported")
    return files


def load_bundle(config_path):
    if not config_path.is_file():
        sys.exit(f"trefoil: config not found: {config_path}")
    packager = Packager()
    bundle = packager.package(config_path)
    if packager.errors:
        print("failed to resolve:", file=sys.stderr)
        for err in packager.errors:
            print(f"  {err}", file=sys.stderr)
        sys.exit(1)
    return bundle


def cmd_package(args):
    out = json.dumps(load_bundle(args.config), indent=2)
    if args.output:
        args.output.write_text(out + "\n")
    else:
        print(out)


def example_files(url, prefix=""):
    """Yields (relative path, download URL) for every file in an example folder on GitHub."""
    with urllib.request.urlopen(url, timeout=30) as resp:
        entries = json.loads(resp.read())
    for e in entries:
        if e["type"] == "dir":
            yield from example_files(e["url"], prefix + e["name"] + "/")
        elif e["type"] == "file":
            yield prefix + e["name"], e["download_url"]


def cmd_init(args):
    dest = args.dir or Path(args.example)
    if dest.exists() and (not dest.is_dir() or any(dest.iterdir())):
        sys.exit(f"trefoil: {dest} already exists and is not empty")
    try:
        files = list(example_files(f"{EXAMPLES_API}/{quote(args.example)}?ref=main"))
        for rel, url in files:
            with urllib.request.urlopen(url, timeout=30) as resp:
                data = resp.read()
            path = dest / rel
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
    except urllib.error.HTTPError as e:
        if e.code == 404:
            sys.exit(f"trefoil: no example named {args.example}")
        sys.exit(f"trefoil: fetching example {args.example}: {e}")
    except urllib.error.URLError as e:
        sys.exit(f"trefoil: cannot fetch example {args.example}: {e.reason}")
    print(f"created {dest} from example {args.example} ({len(files)} files)")
    print(f"next: cd {dest} && trefoil config sync")


def cmd_update(args):
    # An editable install runs straight from a checkout; reinstalling from PyPI would replace it.
    if Path(__file__).with_name("pyproject.toml").exists():
        sys.exit(f"trefoil: running from a source checkout ({Path(__file__).parent}); "
                 "pull and run `make cli` instead")
    # uv-created environments (uv tool install, uv venv) have no pip of their own.
    if importlib.util.find_spec("pip"):
        cmd = [sys.executable, "-m", "pip", "install", "--upgrade", "--force-reinstall", "trefoil-cli"]
    elif shutil.which("uv"):
        cmd = ["uv", "pip", "install", "--python", sys.executable, "--upgrade", "--reinstall", "trefoil-cli"]
    else:
        sys.exit("trefoil: neither pip nor uv is available to reinstall trefoil-cli")
    print(f"trefoil {cli_version()}: {' '.join(cmd)}", file=sys.stderr)
    sys.exit(subprocess.run(cmd).returncode)


def aws_credentials(profile):
    """Resolves a profile (SSO, assumed role, ...) to AWS_* variables with the AWS CLI."""
    if not shutil.which("aws"):
        sys.exit("trefoil: --aws-profile needs the AWS CLI (aws) on PATH")
    out = subprocess.run(["aws", "configure", "export-credentials", "--profile", profile, "--format", "process"],
                         capture_output=True, text=True)
    if out.returncode != 0:
        sys.exit(f"trefoil: aws profile {profile}: {out.stderr.strip() or 'no credentials'}"
                 f" (try: aws sso login --profile {profile})")
    creds = json.loads(out.stdout)
    env = {"AWS_ACCESS_KEY_ID": creds["AccessKeyId"], "AWS_SECRET_ACCESS_KEY": creds["SecretAccessKey"]}
    if creds.get("SessionToken"):
        env["AWS_SESSION_TOKEN"] = creds["SessionToken"]
    if creds.get("Expiration"):
        print(f"trefoil: credentials from profile {profile} expire at {creds['Expiration']}; "
              "restart the engine after that", file=sys.stderr)
    region = subprocess.run(["aws", "configure", "get", "region", "--profile", profile],
                            capture_output=True, text=True).stdout.strip()
    if region:
        env["AWS_REGION"] = region
    return env


def provider_env(args):
    """Returns the variables the engine container needs for args.provider.

    Values go in the docker process's environment and are passed by name
    ("-e NAME"), so they don't show up in the printed command or ps."""
    env = {"LLM_PROVIDER": args.provider}
    if args.provider == "bedrock":
        profile = args.aws_profile or (os.environ.get("AWS_PROFILE")
                                       if not os.environ.get("AWS_ACCESS_KEY_ID") else None)
        if profile:
            env.update(aws_credentials(profile))
        else:
            for name in ("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_BEARER_TOKEN_BEDROCK"):
                if os.environ.get(name):
                    env[name] = os.environ[name]
        if not (env.get("AWS_ACCESS_KEY_ID") or env.get("AWS_BEARER_TOKEN_BEDROCK")):
            sys.exit("trefoil: no AWS credentials for bedrock: pass --aws-profile or set "
                     "AWS_PROFILE, AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY or AWS_BEARER_TOKEN_BEDROCK")
        region = args.aws_region or os.environ.get("AWS_REGION") or os.environ.get("AWS_DEFAULT_REGION") \
            or env.get("AWS_REGION")
        if not region:
            sys.exit("trefoil: no AWS region for bedrock: pass --aws-region or set AWS_REGION")
        env["AWS_REGION"] = region
    elif args.provider == "anthropic":
        key = os.environ.get("ANTHROPIC_API_KEY")
        if not key and sys.stdin.isatty():
            key = getpass.getpass("ANTHROPIC_API_KEY: ").strip()
        if not key:
            sys.exit("trefoil: set ANTHROPIC_API_KEY to use the anthropic provider")
        env["ANTHROPIC_API_KEY"] = key
        if os.environ.get("ANTHROPIC_BASE_URL"):
            env["ANTHROPIC_BASE_URL"] = os.environ["ANTHROPIC_BASE_URL"]
    return env


ENGINE_FILE = Path.home() / ".trefoil" / "engine.json"


def ask(question, default=None, choices=None):
    """Prompts on stderr (stdout stays for results) until the answer is valid."""
    hint = f" [{default}]" if default else ""
    while True:
        sys.stderr.write(f"{question}{hint}: ")
        sys.stderr.flush()
        try:
            answer = input().strip() or default or ""
        except EOFError:
            sys.exit("\ntrefoil: setup cancelled")
        if choices is None and answer:
            return answer
        if choices and answer.isdigit() and 1 <= int(answer) <= len(choices):
            return choices[int(answer) - 1]
        if choices and answer in choices:
            return answer


def pick(question, options, default=None):
    """Asks for one of options (shown numbered); returns the chosen one."""
    for i, option in enumerate(options, 1):
        print(f"  {i}) {option}", file=sys.stderr)
    return ask(question, default if default in options else None, options)


def engine_setup(saved):
    """Asks which LLM backend the engine uses; returns the (non-secret) answers."""
    print("trefoil engine setup (saved to {}, rerun with --setup)".format(ENGINE_FILE), file=sys.stderr)
    labels = {"bedrock": "bedrock (AWS)", "anthropic": "anthropic (API key)"}
    label = pick("LLM backend", list(labels.values()), labels.get(saved.get("provider"), labels["bedrock"]))
    provider = next(k for k, v in labels.items() if v == label)
    choice = {"provider": provider}
    if provider == "bedrock":
        profiles = []
        if shutil.which("aws"):
            profiles = subprocess.run(["aws", "configure", "list-profiles"],
                                      capture_output=True, text=True).stdout.split()
        env_creds = "credentials from environment (AWS_ACCESS_KEY_ID, ...)"
        options = profiles + [env_creds]
        default = saved.get("aws_profile") or os.environ.get("AWS_PROFILE") or ("default" if "default" in profiles else None)
        if not profiles:
            print("  (no AWS CLI profiles found; set AWS_* variables before engine start)", file=sys.stderr)
        profile = pick("AWS credentials", options, default or options[0])
        region = None
        if profile != env_creds:
            choice["aws_profile"] = profile
            region = subprocess.run(["aws", "configure", "get", "region", "--profile", profile],
                                    capture_output=True, text=True).stdout.strip() or None
        choice["aws_region"] = ask("AWS region", saved.get("aws_region") or region
                                   or os.environ.get("AWS_REGION") or "eu-west-1")
    elif not os.environ.get("ANTHROPIC_API_KEY"):
        print("  ANTHROPIC_API_KEY isn't set; you'll be asked for it on each start "
              "(export it to skip that). It is never saved.", file=sys.stderr)
    ENGINE_FILE.parent.mkdir(parents=True, exist_ok=True)
    ENGINE_FILE.write_text(json.dumps(choice, indent=2) + "\n")
    return choice


def apply_engine_choice(args):
    """Fills args.provider/aws_* from ~/.trefoil/engine.json, running setup on
    first use (or with --setup). Flags and --settings win over saved answers."""
    if args.provider and not args.setup:
        return
    try:
        saved = json.loads(ENGINE_FILE.read_text())
    except (OSError, ValueError):
        saved = {}
    if args.setup or (not saved and not args.settings and sys.stdin.isatty()):
        saved = engine_setup(saved)
    if args.settings and not args.setup:
        return
    args.provider = args.provider or saved.get("provider")
    args.aws_profile = args.aws_profile or saved.get("aws_profile")
    args.aws_region = args.aws_region or saved.get("aws_region")


def cmd_engine_start(args):
    if not shutil.which("docker"):
        sys.exit("trefoil: docker is not installed or not on PATH")
    apply_engine_choice(args)
    cmd = ["docker", "run", "--rm", "--name", args.name, "-p", f"{args.port}:8080",
           "-v", "/var/run/docker.sock:/var/run/docker.sock", "-v", f"{args.volume}:/data"]
    if args.settings:
        path = args.settings.resolve()
        if not path.is_file():
            sys.exit(f"trefoil: {args.settings} is not a file")
        mount = f"/trefoil/settings/{path.name}"
        cmd += ["-v", f"{path}:{mount}:ro", "-e", f"TREFOIL_SETTINGS={mount}"]
        # The server resolves env_file against the settings file and fails if
        # it's missing, so mount it where it will look.
        try:
            env_file = (yaml.safe_load(path.read_text()) or {}).get("env_file")
        except (yaml.YAMLError, AttributeError) as e:
            sys.exit(f"trefoil: {args.settings}: {e}")
        if env_file:
            host = (path.parent / env_file).resolve()
            if not host.is_file():
                sys.exit(f"trefoil: env_file {host} (from {args.settings}) not found")
            target = env_file if os.path.isabs(env_file) else os.path.normpath(f"/trefoil/settings/{env_file}")
            cmd += ["-v", f"{host}:{target}:ro"]
    env = provider_env(args) if args.provider else {}
    for name in env:
        cmd += ["-e", name]
    if not args.foreground:
        cmd.append("-d")
    cmd.append(args.image)
    print(f"trefoil: {' '.join(cmd)}", file=sys.stderr)
    code = subprocess.run(cmd, env={**os.environ, **env},
                          stdout=subprocess.DEVNULL if not args.foreground else None).returncode
    if code == 0 and not args.foreground:
        print(f"trefoil: engine running on http://localhost:{args.port} "
              f"(logs: docker logs -f {args.name}, stop: trefoil engine stop)", file=sys.stderr)
    sys.exit(code)


def cmd_engine_stop(args):
    if not shutil.which("docker"):
        sys.exit("trefoil: docker is not installed or not on PATH")
    # started with --rm, so stopping also removes the container
    cmd = ["docker", "stop", args.name]
    print(f"trefoil: {' '.join(cmd)}", file=sys.stderr)
    sys.exit(subprocess.run(cmd, stdout=subprocess.DEVNULL).returncode)


def api_request(server, method, path, body=None, timeout=30, raw=False):
    """Calls the API and returns the decoded JSON, or the bytes with raw=True."""
    req = urllib.request.Request(server.rstrip("/") + path, data=body, method=method)
    if body is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            data = resp.read()
            return data if raw else json.loads(data)
    except urllib.error.HTTPError as e:
        try:
            msg = json.loads(e.read()).get("error", e.reason)
        except ValueError:
            # Our handlers always answer in JSON; a plain-text 404 means the
            # server has no such route, usually because it's an older build.
            if e.code == 404:
                msg = f"{method} {path} not found on {server}; is the server up to date? (restart `make dev`)"
            else:
                msg = e.reason
        sys.exit(f"trefoil: {msg}")
    except urllib.error.URLError as e:
        msg = f"trefoil: cannot reach server at {server}: {e.reason}"
        if isinstance(e.reason, ConnectionRefusedError):
            msg += "\nPerhaps you forgot to start the engine? Run `trefoil engine start`."
        sys.exit(msg)


def cmd_sync(args):
    bundle = load_bundle(args.config)
    result = api_request(args.server, "POST", "/conf", json.dumps(bundle).encode())
    print(f"synced {args.config} to {args.server} ({len(bundle.get('agents', []))} agents)")
    dockerfiles = [f for f in result.get("files", []) if f.endswith("/Dockerfile")]
    for f in dockerfiles:
        print(f"  generated {f}")


def follow_job(server, job_id, interval=1.0):
    """Streams a job's logs to stderr until it finishes; exits 1 if it failed."""
    printed = 0
    while True:
        job = api_request(server, "GET", f"/jobs/{quote(job_id)}")
        sys.stderr.write(job["logs"][printed:])
        sys.stderr.flush()
        printed = len(job["logs"])
        if job["status"] in ("succeeded", "failed"):
            break
        time.sleep(interval)
    if job["status"] == "failed":
        sys.exit(f"trefoil: job {job_id} failed: {job.get('error', '')}")
    print(f"job {job_id} succeeded: {job['agent']} -> {job['image']}")


def cmd_agents_build(args):
    job = api_request(args.server, "POST", f"/agents/{quote(args.agent)}/build", b"")
    if "id" not in job:
        # Servers from before build jobs built synchronously and returned no job id.
        sys.exit(f"trefoil: server at {args.server} doesn't return build jobs; is it up to date? (restart `make dev`)")
    print(f"job {job['id']}: building {job['agent']} -> {job['image']}", file=sys.stderr)
    if args.dockerfile:
        print(job["dockerfile"], file=sys.stderr)
    if args.detach:
        print(job["id"])
        return
    follow_job(args.server, job["id"])


def cmd_agents_context(args):
    data = api_request(args.server, "GET", f"/agents/{quote(args.agent)}/context", timeout=300, raw=True)
    output = args.output or Path(f"{args.agent}-context.zip")
    output.write_bytes(data)
    print(output)


def cmd_jobs_status(args):
    if args.follow:
        follow_job(args.server, args.id)
        return
    job = api_request(args.server, "GET", f"/jobs/{quote(args.id)}")
    print(f"job {job['id']}  {job['kind']}  {job['agent']}  {job['status']}")
    if job.get("context_dir"):
        print(f"context: {job['context_dir']}")
    if job.get("error"):
        print(f"error: {job['error']}")
    if args.logs:
        sys.stdout.write(job["logs"])


def cmd_jobs_list(args):
    rows = [(j["id"], j["kind"], j["agent"], j["status"], j["created_at"][:19].replace("T", " "))
            for j in api_request(args.server, "GET", "/jobs?" + urlencode(
                {k: v for k, v in (("agent", args.agent), ("status", args.status)) if v}))]
    print_table(("ID", "KIND", "AGENT", "STATUS", "CREATED"), rows, empty="no jobs")


def read_input(value):
    """--input value: file://path to a JSON file, or inline JSON."""
    if value.startswith("file://"):
        path = Path(value.removeprefix("file://")).expanduser()
        try:
            value = path.read_text()
        except OSError as e:
            sys.exit(f"trefoil: --input: {e}")
    try:
        data = json.loads(value)
    except ValueError as e:
        sys.exit(f"trefoil: --input is not valid JSON: {e}")
    if not isinstance(data, dict):
        sys.exit("trefoil: --input must be a JSON object")
    return data


def cmd_agents_run(args):
    if args.input is not None:
        if args.task is not None:
            sys.exit("trefoil: pass either a task or --input, not both")
        body = json.dumps(read_input(args.input)).encode()
    else:
        task = args.task if args.task is not None else sys.stdin.read()
        body = json.dumps({"task": task}).encode()
    started = api_request(args.server, "POST", f"/agents/{quote(args.agent)}/run", body)
    if "session_id" not in started:
        sys.exit(f"trefoil: server at {args.server} doesn't return sessions; is it up to date? (restart `make dev`)")
    follow_run(args, started["session_id"])


def follow_run(args, session_id):
    """Follows a session that was just started, or prints its id with -d."""
    print(f"session {session_id}: running {args.agent}", file=sys.stderr)
    if args.detach:
        print(session_id)
        return
    follow_session(args.server, args.agent, session_id)
    print_result(args.server, args.agent, session_id)


def cmd_secrets_set(args):
    # Read from a prompt or stdin, never argv, so the value stays out of shell history.
    if sys.stdin.isatty():
        value = getpass.getpass(f"{args.name}: ")
    else:
        value = sys.stdin.read().rstrip("\n")
    if not value:
        sys.exit("trefoil: secret value is empty")
    api_request(args.server, "POST", f"/secret/{quote(args.name)}", json.dumps({"value": value}).encode())
    print(f"stored secret {args.name} on {args.server}", file=sys.stderr)


def cmd_secrets_list(args):
    secrets = api_request(args.server, "GET", "/secrets")
    rows = [
        (s["name"], "yes" if s["required"] else "no",
         s["source"] or ("missing" if s["required"] else "not set"), ", ".join(s["agents"]))
        for s in secrets
    ]
    print_table(("NAME", "REQUIRED", "SOURCE", "USED BY"), rows, empty="no secrets")


def cmd_params_list(args):
    params = api_request(args.server, "GET", "/params")
    rows = [
        (p["name"], p["type"] + ("" if p["required"] else "?"),
         json.dumps(p["value"]) if "value" in p else "-",
         p["source"] or ("missing" if p["required"] else "not set"), ", ".join(p["agents"]))
        for p in params
    ]
    print_table(("NAME", "TYPE", "VALUE", "SOURCE", "USED BY"), rows, empty="no params")


def cmd_params_set(args):
    # Sent as a string; the server parses it by the param's type.
    value = args.value if args.value is not None else sys.stdin.read().rstrip("\n")
    api_request(args.server, "POST", f"/param/{quote(args.name)}", json.dumps({"value": value}).encode())
    print(f"set param {args.name} on {args.server}", file=sys.stderr)


def cmd_params_unset(args):
    api_request(args.server, "DELETE", f"/param/{quote(args.name)}")
    print(f"unset param {args.name} on {args.server}; it falls back to the config's default", file=sys.stderr)


def session_path(agent, session_id, what):
    return f"/agents/{quote(agent)}/sessions/{quote(session_id)}/{what}"


# The harness's timeline events ("x-trefoil-event: <type> <json>") are for
# the dashboard; the CLI shows the human-readable lines around them.
EVENT_PREFIX = "x-trefoil-event: "


def strip_events(logs):
    return "".join(line for line in logs.splitlines(keepends=True) if not line.startswith(EVENT_PREFIX))


def follow_session(server, agent, session_id, interval=1.0):
    """Streams a session's logs to stderr until it finishes."""
    offset, partial = 0, ""
    while True:
        page = api_request(server, "GET", session_path(agent, session_id, "logs") + f"?offset={offset}")
        # Hold back an unfinished last line until it is complete.
        text = partial + page["logs"]
        done = page["status"] != "running"
        cut = len(text) if done else text.rfind("\n") + 1
        text, partial = text[:cut], text[cut:]
        sys.stderr.write(strip_events(text))
        sys.stderr.flush()
        offset = page["next_offset"]
        if done:
            return
        time.sleep(interval)


def print_result(server, agent, session_id):
    """Prints a finished session's answer to stdout and exits with its code."""
    status = api_request(server, "GET", session_path(agent, session_id, "status"))
    if status.get("exit_code") is None:
        sys.exit(f"trefoil: session {session_id} failed: {status.get('error', '')}")
    result = api_request(server, "GET", session_path(agent, session_id, "result"))
    if "output" in result:
        print(json.dumps(result["output"], indent=2))
    else:
        sys.stdout.write(result["stdout"])
    if status.get("error"):
        print(f"trefoil: session {session_id}: {status['error']}", file=sys.stderr)
    if result.get("artifacts"):
        names = ", ".join(a["path"] for a in result["artifacts"])
        print(f"artifacts: {names} (download: trefoil sessions artifacts --agent={agent} {session_id} -o DIR)",
              file=sys.stderr)
    sys.exit(result["exit_code"])


def cmd_sessions_status(args):
    status = api_request(args.server, "GET", session_path(args.agent, args.id, "status"))
    line = f"session {status['id']}  {status['agent']}  {status['status']}"
    if status.get("exit_code") is not None:
        line += f"  exit {status['exit_code']}"
    print(line)
    if status.get("error"):
        print(f"error: {status['error']}")


def cmd_sessions_logs(args):
    if args.output is not None:
        data = api_request(args.server, "GET", session_path(args.agent, args.id, "logs") + "?download=1",
                           timeout=300, raw=True)
        args.output.write_bytes(data)
        print(args.output)
        return
    if args.follow:
        follow_session(args.server, args.agent, args.id)
        return
    sys.stdout.write(strip_events(api_request(args.server, "GET", session_path(args.agent, args.id, "logs"))["logs"]))


def cmd_sessions_result(args):
    print_result(args.server, args.agent, args.id)


def cmd_sessions_rerun(args):
    started = api_request(args.server, "POST", session_path(args.agent, args.id, "rerun"))
    print(f"rerunning session {args.id} with the same input", file=sys.stderr)
    follow_run(args, started["session_id"])


def cmd_sessions_stop(args):
    api_request(args.server, "POST", session_path(args.agent, args.id, "stop"))
    print(f"stopping session {args.id}; it finishes as failed with error \"stopped\"", file=sys.stderr)


def cmd_sessions_artifacts(args):
    artifacts = api_request(args.server, "GET", session_path(args.agent, args.id, "artifacts"))["artifacts"]
    if args.output is None:
        print_table(("PATH", "SIZE"), [(a["path"], str(a["size"])) for a in artifacts], empty="no artifacts")
        return
    for a in artifacts:
        # The server only lists relative paths, but don't trust it to write outside DIR.
        dest = (args.output / a["path"]).resolve()
        if not dest.is_relative_to(args.output.resolve()):
            sys.exit(f"trefoil: refusing artifact path outside {args.output}: {a['path']}")
        data = api_request(args.server, "GET", session_path(args.agent, args.id, "artifacts/" + quote(a["path"])),
                           timeout=300, raw=True)
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_bytes(data)
        print(dest)
    if not artifacts:
        print("no artifacts", file=sys.stderr)


def cmd_agents_list(args):
    agents = api_request(args.server, "GET", "/agents")

    rows = [
        (a["name"], a.get("model") or "(default)", a.get("provider") or "anthropic", ", ".join(a["modules"]),
         format_input_schema(a.get("input_schema")), format_fields(a.get("outputs")), str(a["prompts"]),
         "yes" if a["built"] else "no")
        for a in agents
    ]
    print_table(("NAME", "MODEL", "PROVIDER", "MODULES", "INPUT", "OUTPUTS", "PROMPTS", "BUILT"), rows,
                empty="no agents")


def format_input_schema(schema):
    """{"properties": {"repo": {"type": "string"}}, "required": ["repo"]} -> "repo: string"."""
    schema = schema or {}
    required = schema.get("required") or []
    out = []
    for name, prop in (schema.get("properties") or {}).items():
        typ = prop.get("type") if isinstance(prop.get("type"), str) else "any"
        if typ == "array" and isinstance((prop.get("items") or {}).get("type"), str):
            typ = prop["items"]["type"] + "[]"
        out.append(f"{name}: {typ}" + ("" if name in required else "?"))
    return ", ".join(out)


def format_fields(fields):
    """[{"statement": "string"}] -> "statement: string"."""
    return ", ".join(f"{k}: {v}" for f in fields or [] for k, v in f.items())


def print_table(header, rows, empty):
    if not rows:
        print(empty)
        return
    widths = [max(len(r[c]) for r in [header, *rows]) for c in range(len(header))]
    for row in [header, *rows]:
        print("  ".join(cell.ljust(w) for cell, w in zip(row, widths)).rstrip())


def cli_version():
    try:
        return importlib.metadata.version("trefoil-cli")
    except importlib.metadata.PackageNotFoundError:
        return "unknown"


def engine_image():
    """The API image released with this CLI (same version), or :latest when unknown."""
    version = cli_version()
    return f"{ENGINE_REPO}:{'latest' if version == 'unknown' else version}"


def main():
    parser = argparse.ArgumentParser(prog="trefoil", description="trefoil <domain> <command> [args]")
    parser.add_argument("--version", action="version", version=f"%(prog)s {cli_version()}")
    domains = parser.add_subparsers(dest="domain", metavar="<domain>", required=True)

    server = argparse.ArgumentParser(add_help=False)
    server.add_argument("--server", default=os.environ.get("TREFOIL_SERVER", DEFAULT_SERVER),
                        help=f"API URL (default: $TREFOIL_SERVER or {DEFAULT_SERVER})")
    agent = argparse.ArgumentParser(add_help=False)
    agent.add_argument("--agent", required=True, metavar="NAME", help="agent name")

    def domain(name, help_text):
        return domains.add_parser(name, help=help_text).add_subparsers(
            dest="command", metavar="<command>", required=True)

    init = domains.add_parser("init", help="scaffold a project from an example config")
    init.add_argument("dir", type=Path, nargs="?", help="directory to create (default: the example's name)")
    init.add_argument("--example", default="starter", metavar="NAME",
                      help="example under examples/ in github.com/alto-se/trefoil (default: starter)")
    init.set_defaults(func=cmd_init)

    update = domains.add_parser("update", help="reinstall the latest trefoil-cli from PyPI")
    update.set_defaults(func=cmd_update)

    engine = domain("engine", "run the trefoil API server locally with docker")
    engine_start = engine.add_parser("start", help="run the trefoil API server image")
    engine_start.add_argument("--settings", type=Path, metavar="FILE",
                               help="backend settings file (backend.yml) to mount into the container")
    engine_start.add_argument("--provider", choices=["bedrock", "anthropic"],
                               help="LLM backend agents use (overrides llm_provider in --settings); "
                                    "bedrock takes AWS credentials, anthropic ANTHROPIC_API_KEY")
    engine_start.add_argument("--setup", action="store_true",
                               help=f"ask for the LLM backend again (answers are saved to {ENGINE_FILE})")
    engine_start.add_argument("--aws-profile", metavar="PROFILE",
                               help="AWS profile to take bedrock credentials and region from "
                                    "(default: $AWS_PROFILE unless AWS_ACCESS_KEY_ID is set)")
    engine_start.add_argument("--aws-region", metavar="REGION",
                               help="AWS region for bedrock (default: $AWS_REGION or the profile's region)")
    engine_start.add_argument("--port", type=int, default=8080, help="host port to publish (default: 8080)")
    engine_start.add_argument("--image", default=engine_image(), help=f"API image (default: {engine_image()})")
    engine_start.add_argument("--name", default="trefoil-api", help="container name (default: trefoil-api)")
    engine_start.add_argument("--volume", default="trefoil-data",
                               help="docker volume (or host path) for the data dir (default: trefoil-data)")
    engine_start.add_argument("--foreground", action="store_true",
                              help="run attached instead of in the background")
    engine_start.set_defaults(func=cmd_engine_start)
    engine_stop = engine.add_parser("stop", help="stop the running API server container")
    engine_stop.add_argument("--name", default="trefoil-api", help="container name (default: trefoil-api)")
    engine_stop.set_defaults(func=cmd_engine_stop)

    config = domain("config", "package and upload agent configs")
    package = config.add_parser("package", help=__doc__.splitlines()[0])
    package.add_argument("config", type=Path, help="path to config.yml")
    package.add_argument("-o", "--output", type=Path, help="output file (default: stdout)")
    package.set_defaults(func=cmd_package)
    sync = config.add_parser("sync", parents=[server], help="package a config and upload it to the server")
    sync.add_argument("config", type=Path, nargs="?", default=Path("config.yml"),
                      help="path to config.yml (default: ./config.yml)")
    sync.set_defaults(func=cmd_sync)

    agents = domain("agents", "work with agents on the server")
    agents_list = agents.add_parser("list", parents=[server], help="list agents in the uploaded config")
    agents_list.set_defaults(func=cmd_agents_list)

    agents_build = agents.add_parser("build", parents=[server, agent], help="build an agent's Docker image on the server")
    agents_build.add_argument("-d", "--detach", action="store_true",
                              help="print the job id and return instead of following the build")
    agents_build.add_argument("--dockerfile", action="store_true", help="print the generated Dockerfile")
    agents_build.set_defaults(func=cmd_agents_build)

    agents_context = agents.add_parser("get-dockerfile", aliases=["context"], parents=[server, agent],
                                       help="download an agent's build context (Dockerfile and files) as a zip")
    agents_context.add_argument("-o", "--output", type=Path, metavar="FILE",
                                help="where to save the zip (default: ./NAME-context.zip)")
    agents_context.set_defaults(func=cmd_agents_context)


    agents_run = agents.add_parser("run", parents=[server, agent], help="run a built agent with a task")
    agents_run.add_argument("task", nargs="?", help="task for the agent (default: read from stdin)")
    agents_run.add_argument("--input", metavar="JSON|file://PATH",
                            help="structured input for agents with an input_schema: inline JSON or file://path.json")
    agents_run.add_argument("-d", "--detach", action="store_true",
                            help="print the session id and return instead of following the run")
    agents_run.set_defaults(func=cmd_agents_run)

    sessions = domain("sessions", "follow agent runs on the server")
    for name, func, help_text in (("status", cmd_sessions_status, "show a session's status"),
                                  ("logs", cmd_sessions_logs, "print a session's logs"),
                                  ("result", cmd_sessions_result, "print a finished session's answer"),
                                  ("stop", cmd_sessions_stop, "stop a running session"),
                                  ("rerun", cmd_sessions_rerun, "run a session's agent again with the same input"),
                                  ("artifacts", cmd_sessions_artifacts, "list or download a session's artifacts")):
        sub = sessions.add_parser(name, parents=[server, agent], help=help_text)
        sub.add_argument("id", help="session id")
        sub.set_defaults(func=func)
        if name == "logs":
            sub.add_argument("-f", "--follow", action="store_true", help="stream logs until the session finishes")
            sub.add_argument("-o", "--output", type=Path, metavar="FILE",
                             help="save the full log, timeline events included, to FILE")
        if name == "artifacts":
            sub.add_argument("-o", "--output", type=Path, metavar="DIR",
                             help="download every artifact into DIR instead of listing them")
        if name == "rerun":
            sub.add_argument("-d", "--detach", action="store_true",
                             help="print the new session id and return instead of following the run")

    secrets = domain("secrets", "provide secrets that agents' modules require")
    secrets_list = secrets.add_parser("list", parents=[server],
                                      help="list the secrets agents need and whether they're set (never values)")
    secrets_list.set_defaults(func=cmd_secrets_list)
    secrets_set = secrets.add_parser("set", parents=[server],
                                     help="store a secret on the server (value from a prompt or stdin)")
    secrets_set.add_argument("--name", required=True, metavar="NAME", help="secret name, e.g. github_token")
    secrets_set.set_defaults(func=cmd_secrets_set)

    params = domain("params", "set the config-wide values agents' inputs default to")
    params_list = params.add_parser("list", parents=[server], help="list the config's params and their values")
    params_list.set_defaults(func=cmd_params_list)
    params_set = params.add_parser("set", parents=[server], help="set a param (value as an argument or on stdin)")
    params_set.add_argument("--name", required=True, metavar="NAME", help="param name, e.g. repo")
    params_set.add_argument("value", nargs="?", help="value, parsed by the param's type (JSON for lists and objects)")
    params_set.set_defaults(func=cmd_params_set)
    params_unset = params.add_parser("unset", parents=[server], help="unset a param so it uses the config's default")
    params_unset.add_argument("--name", required=True, metavar="NAME", help="param name")
    params_unset.set_defaults(func=cmd_params_unset)

    jobs = domain("jobs", "track background jobs on the server")
    jobs_list = jobs.add_parser("list", parents=[server], help="list jobs, newest first")
    jobs_list.add_argument("--agent", metavar="NAME", help="only jobs for this agent")
    jobs_list.add_argument("--status", choices=["queued", "running", "succeeded", "failed"])
    jobs_list.set_defaults(func=cmd_jobs_list)
    jobs_status = jobs.add_parser("status", parents=[server], help="show a job's status")
    jobs_status.add_argument("id", help="job id")
    jobs_status.add_argument("-f", "--follow", action="store_true", help="stream logs until the job finishes")
    jobs_status.add_argument("--logs", action="store_true", help="print the logs so far")
    jobs_status.set_defaults(func=cmd_jobs_status)

    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
