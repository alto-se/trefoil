# trefoil

**A metaframework for cloud-based AI agents.**

trefoil gives you a simple way to build the harness around your agents: the
tools they can call, the skills they know, the prompts, limits and secrets
they run with. You describe an agent in YAML, trefoil turns it into a Docker
image with a ready-made agent loop, and runs it as an isolated, observable
session. Develop and test locally, then deploy the same config to the cloud.

```yaml
agents:
  - name: writer
    bricks: [core:os]            # Read, Write and Bash tools
    input_schema:
      type: object
      properties:
        topic: { type: string }
      required: [topic]
    prompts:
      - source: !Ref file://prompt.md
        role: system

defaults:
  provider: bedrock
  model: claude-sonnet-4-6
  limits: { max_turns: 5, timeout_seconds: 120, max_cost_usd: 0.20 }
```

```sh
trefoil config sync config.yml
trefoil agents build --agent=writer
trefoil agents run --agent=writer "a short note on trefoil knots"
```

## Why

Most of the work in shipping an agent isn't the model call, it's everything
around it: a sandbox to run tools in, a loop that handles tool calls and
limits, secrets, logs, files the agent produces, and a way to run it the same
way on your laptop and in production. trefoil packages that into one
config-driven workflow:

- **Declarative agents.** Prompts, input schema, model, provider and limits
  (turns, timeout, cost) live in a YAML config. Split it across files with
  `!Ref`.
- **Bricks.** Reusable building blocks of tools, scripts, image layers, skills,
  secrets and MCP servers. Compose an agent from shared bricks
  (`core:os`, `code:github`, `docs:opendoc`, ...) or define local ones next to
  your config. The name comes from *bricolage*, not bricks like a house.
  Really funny joke I know. Took me several prompts to find a good word for it.
- **Built-in harness.** Every agent image ships with the trefoil harness: the
  agent loop, tool dispatch, Agent Skills, MCP clients and a timeline of
  events (model calls, tool calls, results, limits).
- **Isolated runs.** Each run is its own container. Logs stream live, files
  saved to `/trefoil/artifacts` are collected when the run ends, and secrets
  are injected only at run time.
- **Local first, cloud ready.** The same server, CLI and dashboard run on your
  machine with `make dev`; the build context for any agent can be exported as
  a plain Docker build (`trefoil agents get-bundle`) to run anywhere.

## Concepts

| Concept    | What it is |
|------------|------------|
| **Config** | A YAML file with `agents`, `defaults`, local `bricks` and `params`. Synced to the server with `trefoil config sync`. |
| **Agent**  | A named agent: prompts, bricks, input schema, model and limits. Built into a Docker image. |
| **Brick**  | Tools (Python code with a JSON input schema), image layers, skills, secrets and MCP servers that an agent pulls in. Shared bricks live in `bricks/<namespace>/<name>/`. |
| **Skill**  | An Agent Skill folder (`SKILL.md` plus files) installed in the image and offered to the model through a `Skill` tool. |
| **Session**| One run of an agent: status, logs, event timeline, result and artifacts. |
| **Job**    | A background task on the server, such as an image build. |
| **Params / Secrets** | Server-side values: params fill matching input fields across agents, secrets become env vars in the run container. |

## Quick start

Requirements: Go, Docker, [uv](https://docs.astral.sh/uv/), and credentials
for a model provider (AWS Bedrock or Anthropic).

```sh
make cli            # install the trefoil CLI
make dev            # start the API and dashboard on http://localhost:8080

cd examples/echo
trefoil config sync config.yml
trefoil agents build --agent=echo
trefoil agents run --agent=echo --input=file://input.json
```

Open http://localhost:8080 for the dashboard: agents, run forms generated
from each input schema, live logs, timelines, artifacts, params and jobs.

Server settings (port, AWS region, providers, data dir, `.env` file) are in
`backend.yml`.

## CLI at a glance

```sh
trefoil init [DIR] [--example=NAME]   # scaffold from examples/ on GitHub (default: echo)
trefoil agents list
trefoil agents build   --agent=NAME
trefoil agents run     --agent=NAME "task text"     # or --input=file://input.json, -d to detach
trefoil sessions logs  --agent=NAME SESSION_ID -f
trefoil sessions result --agent=NAME SESSION_ID
trefoil sessions stop   --agent=NAME SESSION_ID   # kill a running session
trefoil sessions artifacts --agent=NAME SESSION_ID -o out/
trefoil secrets set --name=github_token
trefoil params set  --name=repo owner/name
trefoil jobs list
```

## Bricks included

| Namespace | Bricks |
|-----------|--------|
| `core`    | `os` (Read/Write/Bash), `trefoil` (SaveArtifact), `chromium`, `websearch` |
| `code`    | `github`, `go`, `java`, `node`, `python`, `posthog` |
| `cloud`   | `aws` (AWS MCP Server, SigV4 signed) |
| `data`    | `postgres` |
| `docs`    | `opendoc` (office documents via LibreOffice), `release-notes` |
| `style`   | `caveman` |

Each shared brick can carry a `tests.py` that runs inside its real image:
`make test-bricks BRICKS="core:os"`.

## Examples

`examples/` has a config per use case: `echo` (minimal), `writer` (file
tools), `local` (local bricks and split configs), `github`, `aws`,
`postgres`, `pdf` (browse and print with Chromium), `opendoc`,
`release-notes`, `humanizer` (skill from a GitHub zip), `repo-ops` (params
shared by several agents), `error-triage` and language toolchains (`go`,
`node`, `python`).

`make verify` syncs and builds every example against the dev server.

## Repository layout

```
api/        Go API server, dashboard and agent harness
cli/        Python trefoil CLI
bricks/     shared bricks
examples/   sample configs
backend.yml server settings for make dev
```

## Development

```sh
make dev           # API with live reload
make test          # Go tests
make test-bricks   # brick tests in their images
make verify        # build every example
```

See [AGENTS.md](AGENTS.md) for detailed notes on params, persistence, build
contexts, run events, artifacts, secrets, MCP servers and skills.
