---
name: trefoil-agents
description: Use when creating or changing a trefoil agent, module or config (config.yml with agents/modules/defaults/params), or when deciding which tools, layers, skills or MCP servers an agent needs.
---

# Building trefoil modules and agents

An agent is a config entry: name, modules, input_schema, prompts, limits.
Modules give it capabilities: `layers` (build-time shell), `tools`, `skills`,
`mcps`, `server_tools`, `secrets`. Most of the work in a new agent should be
the **prompt**, not new code.

## Rule 1: don't build tools

Writing a new tool is the last resort. Before adding anything to `tools:`, go
down this list and stop at the first thing that works:

1. **An existing shared module** (table below). `core:os` Bash already runs any
   CLI, script or `curl`; `code:python` runs Python with pip packages.
2. **A layer + the prompt.** Need `jq`, `ffmpeg`, `gh`, `pandoc`? Add a
   `layers:` entry that installs it and tell the agent in the prompt to call it
   through Bash.
3. **An MCP server** (`mcps:`) if the service already offers one (GitHub, AWS).
4. **A server tool** (`server_tools: [{name: web_search}]` / `web_fetch`) for
   web access — runs on the model provider, no code.
5. **A skill** (`skills:`) for procedures, conventions, templates and scripts
   the agent runs with Bash. A skill is docs + scripts, not a tool.
6. **Only then a tool** — when the action needs a secret the model must not
   see, has to be atomic/safe (e.g. read-only SQL), or would be long and
   error-prone for the model to repeat by hand every run. Keep it small; one
   function per tool in `tools.py`, `def Name(args): return str`.

If you do write a tool, say why the options above didn't fit.

### Too many, too specific tools

Signs a module has gone wrong — don't write or recommend modules like this:

- **Many tools in one module** (5+), several hundred lines of `tools.py` with
  private helpers, shared state hacks (`sys.modules` tricks to keep a process
  alive between tools) and a long-running daemon behind them.
- **Tools named after one product or workflow**: `OpenPace`,
  `OpenDocsPullRequest`, `CheckGuide`. Each is a step of one agent's job,
  baked into code.
- **Hard-coded environment** at the top of `tools.py`: domains, auth hosts,
  repo paths, content folders, image widths.
- **Re-implementing what exists**: a git/GitHub-API PR tool when `code:github`
  + `git` in Bash does it; a browser wrapper when `core:chromium` exists; a
  content linter as a tool when it could be a script in a skill.

Instead, split by capability and push the workflow into the prompt:

| Specific tool | Generic replacement |
|---|---|
| `OpenPace`, `Browser`, `Screenshot` | one browser module (or `core:chromium`) with allowed hosts and login from params/secrets |
| `StartRecording`/`StopRecording` + ffmpeg | `ffmpeg` installed by a layer, called through Bash |
| `CheckGuide` | a skill with the guide conventions and a `check.py` the agent runs |
| `OpenDocsPullRequest` | `code:github` (`CloneRepo`) + `git` through Bash with `GITHUB_TOKEN` (add a layer for `gh` if needed) |

Aim for few tools with general inputs (`url`, `path`, `sql`, `command`);
the prompt says which hosts, repos and folders this agent works on.

## Rule 2: use core modules when they exist

Always check `modules/` first (`find modules -name config.yml`) — the list
grows. Current shared modules:

| Module | Gives the agent |
|---|---|
| `core:os` | `Read`, `Write`, `Bash` — almost every agent wants this |
| `core:trefoil` | `SaveArtifact` — files the user downloads after the run |
| `core:chromium` | headless Chromium: `OpenPage`, `PrintPdf`, browser skill |
| `code:github` | GitHub MCP (repos/issues/PRs, read-only), `CloneRepo`; needs `github_token` |
| `code:python` | venv at `/opt/venv`, `RunPython` (with pip packages) |
| `code:node` | Node.js LTS, `RunNode` |
| `code:go` | Go toolchain, `RunGo` |
| `data:postgres` | psql, `Query` (read-only unless `write: true`); needs `postgres_url` |
| `docs:opendoc` | LibreOffice: `ReadDocument`, `ConvertDocument`, `CreateDocument` + ODF skills (needs `core:os`) |
| `docs:release-notes` | release-notes skill |
| `code:trefoil` | this skill, for agents that write trefoil configs |
| `cloud:aws` | AWS MCP Server, SigV4-signed with the container's AWS credentials |
| `style:caveman` | terse-reply skill |

`code:posthog` and `core:websearch` are unfinished stubs — don't use them.

Modules shared by every agent in a config go in `defaults.modules`; per-agent
ones in the agent's `modules:`. Need something a shared module almost covers?
Add a **local** module (no `ns:` prefix, under top-level `modules:`) with just
the missing layer — don't copy the shared module.

## Rule 3: keep modules generic

A module is a capability, not a part of one agent. Write it so the next agent
can list it unchanged:

- **Name it for what it does**, not who uses it: `browser`, `sentry`,
  `log-search` — not `bug-fix-browser` or `error-triage-tools`.
- **No agent logic inside.** Hosts, repos, projects, queries, thresholds and
  credentials come from params, secrets, env or tool input — never hard-coded
  in the layer, tool or skill. The agent's workflow belongs in its prompt.
- **Small and focused.** One capability per module; an agent combines several.
  Split a module that mixes unrelated things (a CLI install plus one agent's
  report format).
- **Generic tool shapes**: `Query(sql)`, `OpenPage(url)` over
  `GetYesterdaysCheckoutErrors()`.

An agent-specific module is OK only when the capability truly can't be
reused (a one-off parser for one system's format). Even then keep it local
(`trefoil/modules/<name>/`), and promote it to a shared module once a second
agent needs it.

## Bad pattern: an app hidden in a layer script

Don't write programs into a layer with heredocs — `cat > package.json`,
`cat > package-lock.json`, `cat > skill.mjs <<EOF ... EOF`, then `npm ci`.
Hundreds of lines of source and lockfile inside a YAML string can't be
linted, tested, diffed or reused, and the capability is welded to one agent.

Make it a module of its own instead (e.g. `modules/browser/`), so any agent can
list it:

- **Check first** whether a shared module already covers it (`core:chromium`
  for a headless browser).
- **Layer = dependencies only**, short and pinned: `apt-get install chromium`,
  `npm install --prefix /opt/browser puppeteer-core@25.12.0`,
  `pip install ...`. Layers run before tools and skills are copied in, so a
  layer can't build from the module's own files.
- **Code = real files in the module**: a skill folder/zip with the script
  (`skills/browser/SKILL.md` + `browser.mjs`, installed at
  `/trefoil/skills/browser/`, run with Bash), or `tools.py` if it must be a
  tool (Rule 1).
- **Config through env/secrets** (`secrets:`), never typed by the model —
  credentials in particular.
- **`tests.py`** next to `config.yml` for shared modules (`make test-modules`).

## Folder layout

A project that keeps its trefoil config in its own repo uses:

```
trefoil/
  config.yml                 # params, defaults, agents: !include, modules: !include
  agents/<name>/config.yml   # one agent per folder
  agents/<name>/prompt.md
  modules/<name>/config.yml   # local modules, laid out like shared ones
  modules/<name>/tools.py
  modules/<name>/skills/<skill>.zip
```

```yaml
# trefoil/config.yml
agents:
  - !include file://agents/error-triage/config.yml
modules:
  log-triage: !include file://modules/log-triage/config.yml
```

Refs resolve against the referencing file's folder, so an agent's
`prompt.md` is `!include file://prompt.md`.

## Where things go

- Agent only for this config → local module in `trefoil/modules/<name>/`
  (`modules: {name: !include file://modules/name/config.yml}`), see `examples/local`.
- Reusable across configs → shared module `modules/<ns>/<name>/config.yml`
  (+ `tools.py`, `skills/*.zip`, `tests.py`), referenced as `ns:name`.
- Values the user sets once (a repo, a project id) → top-level `params`, not
  hard-coded in prompts. See `examples/repo-ops`.
- Credentials → module `secrets:` (`trefoil secrets set --name=...`), never in
  config or prompts.

## Agent template

```yaml
agents:
  - name: my-agent
    modules: [core:os, core:trefoil]
    input_schema:
      type: object
      properties:
        task: { type: string, description: "What to do" }
      required: [task]
    prompts:
      - source: !include file://prompts/my-agent.md
        role: system
    limits: { max_turns: 20, timeout_seconds: 600, max_cost_usd: 1.00 }

defaults:
  provider: bedrock
  aws_region: eu-west-1
  model: "claude-sonnet-4-6"
  max_tokens: 8000
```

The prompt should name the tools/CLIs to use, the steps, where to write output
(`/workspace/...`, then `SaveArtifact`) and what the final answer looks like.

## Layer / tool / skill shapes

```yaml
layers:
  - title: "Install jq"
    description: "jq from Debian"
    script: |
      apt-get update
      apt-get install -y --no-install-recommends jq
      rm -rf /var/lib/apt/lists/*
      jq --version            # fail the build early if it didn't install

skills:
  - source: !include file://skills/my-skill.zip   # zip -rX my-skill.zip my-skill

tools:                                          # last resort, see Rule 1
  - name: DoThing
    description: "What it does and what it returns"
    input_schema:
      type: object
      properties: { x: { type: string, description: "..." } }
      required: [x]
    command:
      language: python
      code: { content: !include file://tools.py, entry: DoThing }
```

## Verify

```sh
trefoil config sync path/to/config.yml       # validates against the schema
trefoil agents build --agent=my-agent
trefoil agents run --agent=my-agent "task text"
make test-modules MODULES="ns:name"            # if you added/changed a shared module
make verify EXAMPLES="examples/x"            # if you added an example
```

Check the run's timeline (`/#runs/<id>`) — if the agent fumbles a step every
run, fix the prompt or add a skill before reaching for a tool. Update
`AGENTS.md` when you add a shared module.
