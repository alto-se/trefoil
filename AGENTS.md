# AGENTS.md

Notes for coding agents working in this repo.

## Layout

- `api/` — Go API server (`api/cmd/api`), internal packages under `api/internal/` (conf, bricks, build, jobs, sessions, server, settings, store, schema).
- `cli/` — Python `trefoil` CLI, single module `cli/package.py`.
- `bricks/` — shared bricks (tools, scripts and skills used to build agent images).
- `examples/` — sample configs, e.g. `examples/echo/`.
- `backend.yml` — server settings used by `make dev`.

## Building agents and bricks

Read `bricks/code/trefoil/skills/trefoil-agents/SKILL.md` before writing an
agent or brick (Claude Code loads it through the `.claude/skills/` symlink;
trefoil agents get it from the `code:trefoil` brick): prefer shared bricks (`core:os`, `code:github`, ...) over new tools,
keep bricks generic (a capability, not one agent's part), put programs in their own brick instead of heredocs in a layer script, and
lay configs out as `trefoil/agents/<name>/` and `trefoil/bricks/<name>/`.

## Common tasks

```sh
make dev     # run the API with live reload
make test    # go test ./... in api/
make test-bricks [BRICKS="core:os code:go"]  # run bricks' tests.py in their images
make verify  [EXAMPLES="examples/go"]       # sync + build every example (scripts/verify)
make cli     # install the trefoil CLI (editable, via uv)
make seed    # trefoil config sync $(CONFIG)
```

## Releases

Pushes to `main` are released by `.github/workflows/release.yml`:
semantic-release (config in `.releaserc.json`) derives the version from
conventional commits since the last `v*` tag (`fix:` bumps patch, `feat:`
bumps minor, `feat!:` or `BREAKING CHANGE:` bumps major; other types such as
`chore:` and `docs:` release nothing), commits `VERSION` and
`cli/pyproject.toml` back to `main` and tags `v<version>`. Then, from that
tag, the CLI is published to PyPI as `trefoil-cli` (trusted publishing) and
the API image to `ghcr.io/alto-se/trefoil-api:<version>`, `:<major>.<minor>`
and `:latest`. Never edit the version by hand. Pull requests run
`.github/workflows/ci.yml` (`go test`, image build, CLI build and smoke test).
Install the CLI with `pip install trefoil-cli` (the command is `trefoil`).

## API image

`Dockerfile` builds the API server with the shared bricks baked in
(`BRICKS_DIR=/trefoil/bricks`, `DATA_DIR=/data`). It calls the docker CLI to
build and run agents, so mount the host socket:
`docker run -p 8080:8080 -v /var/run/docker.sock:/var/run/docker.sock -v trefoil-data:/data ghcr.io/alto-se/trefoil-api`.

## CLI shape

The CLI is always `trefoil <domain> <command> [args]`. Named things are passed
as flags (`--agent=NAME`), not positionals. Every server-talking command takes
`--server URL` (default `$TREFOIL_SERVER` or `http://localhost:8080`), placed
after the command.

```sh
trefoil --version
trefoil update                     # reinstall the latest trefoil-cli via pip (or uv)
trefoil config package path/to/config.yml [-o bundle.json]
trefoil config sync [path/to/config.yml]

trefoil agents list
trefoil agents build --agent=echo [-d] [--dockerfile]
trefoil agents get-bundle --agent=echo [-o FILE]       # build context as a zip
trefoil agents run --agent=echo --input=file://input.json [-d]
trefoil agents run --agent=echo "task text"        # or pipe the task on stdin

trefoil sessions status --agent=echo SESSION_ID
trefoil sessions logs   --agent=echo SESSION_ID [-f] [-o FILE]
trefoil sessions result --agent=echo SESSION_ID
trefoil sessions stop   --agent=echo SESSION_ID
trefoil sessions artifacts --agent=pdf SESSION_ID [-o DIR]

trefoil secrets set --name=github_token          # value from prompt or stdin
trefoil params list
trefoil params set --name=repo owner/name        # or value on stdin
trefoil params unset --name=repo

trefoil jobs list [--agent=echo] [--status=running]
trefoil jobs status JOB_ID [-f] [--logs]
```

When adding a command: put it under an existing domain (or add a new domain
with the `domain()` helper in `main()`), reuse the shared `server` / `agent`
parent parsers, and keep logs on stderr and results on stdout.

## Example: echo agent

```sh
cd examples/echo
trefoil config sync config.yml
trefoil agents build --agent=echo
trefoil agents run --agent=echo --input=file://input.json
```

## Params

A config's top-level `params` declares config-wide values, written like an
`input_schema` (`type: object`, `properties`, `required`). Values are set on
the server, not in the config: `POST /param/{name}` (`{"value": ...}`; a
string is parsed by the param's type), `DELETE /param/{name}`, `GET /params`,
`trefoil params list|set|unset`, or the dashboard's Params page, which uses
the same form controls as a run's input. They're stored in
`<data dir>/params.json` and survive config syncs; an unset param falls back
to its property's `default`. When a run leaves out an `input_schema`
property named after a param, the server fills it in before validating
(`Conf.FillParams`), so a value passed in the run still wins; a required
param with no value rejects the run with 422 and `missing_params: [...]`.
Run forms on the dashboard start with the param's value. See
`examples/repo-ops`, where the `repo` param picks the repository for five
agents (tests, release-notes, license, triage, deps-audit).

## Persistence

Everything lives under the data dir (`storage.local.path`, `./.data` in
`backend.yml`): `conf.json`, `params.json`, `secrets/`, build contexts in
`builds/<agent>/<job id>/`, and jobs and sessions as `jobs/<id>.json` and
`sessions/<id>.json` with logs appended live to `<id>.log` next to them. The
server loads them on start; anything still queued or running is marked failed
with "interrupted by server restart".

## Build contexts

`GET /agents/{name}/context` composes the agent's build context from the
stored config (as a build would) and sends it as `<agent>-context.zip`: the
Dockerfile and every file it copies under one `<agent>/` folder, so
`unzip` then `docker build <agent>` builds the same image outside the
server. `trefoil agents get-bundle --agent=NAME [-o FILE]` saves it, and the
dashboard's agent page links to it.

## Stopping runs

`POST /agents/{name}/sessions/{id}/stop` (`trefoil sessions stop`, or the
Stop button on the dashboard's run page) cancels the run's context, which
`docker kill`s its container; artifacts are still collected. The session
finishes as `failed` with error `stopped` and a `[trefoil] run stopped by
request` line in its logs. Stopping a session that isn't running is a 409.

## Run timeline events

The harness writes timeline events to stderr as single lines,
`x-trefoil-event: <type> <json>` (types: start, model, tool_call,
tool_result, server_tool, done, limit, error; payloads carry `ts` and
tool_call/tool_result share `id`). The server parses them from the session
logs (`sessions.ParseEvents`) for the dashboard's run page (`/#runs/<id>`) and
`GET /agents/{name}/sessions/{id}/events`; the dashboard and CLI hide those
lines from the human-readable logs. `.../logs?download=1` sends the full
raw log (events included) as `<agent>-<id>.log`; the run page's Logs
section links to it and `trefoil sessions logs -o FILE` saves it. Agents must be rebuilt to pick up harness
changes.

Tool results sent to the model are capped at `TOOL_OUTPUT_MAX` (30,000 chars)
in the harness: longer ones keep their first and last 15,000 chars with a note
in between, so one noisy command can't overflow the context window.

## Artifacts

Tools save files for the user by putting them in `/trefoil/artifacts` in the
container; the `core:trefoil` brick's `SaveArtifact` tool copies a path there.
Run containers aren't started with `--rm`: when the run ends (even failed or
timed out) the server `docker cp`s that directory to
`<data dir>/sessions/<id>/artifacts/`, then removes the container. Stopped run
containers left by a server restart (label `trefoil.run=1`) are pruned on
start. List them with `GET /agents/{name}/sessions/{id}/artifacts` (also in
`result` as `artifacts`), fetch one with `.../artifacts/{path}` (served with
`CSP: sandbox`). See `examples/pdf`, which uses `core:chromium` to browse and
print a page to PDF.

`docs:opendoc` handles office files with headless LibreOffice: `ReadDocument`
(odt/ods/odp, and docx/xlsx/pptx by converting), `ConvertDocument` and
`CreateDocument` (HTML to odt/docx/pdf, CSV to ods/xlsx). Its skills
(`odf-text`, `odf-spreadsheet`, `odf-presentation`) carry odfpy scripts run
with `/opt/odf/bin/python`, so agents also need `core:os`. See
`examples/opendoc`.

## Secrets and MCP servers

A brick declares `secrets` (e.g. `github_token: {required: true}`); at run
time the server sets `GITHUB_TOKEN` in the container from the value stored
with `POST /secret/github_token` (`{"value": "..."}`, written to
`<data dir>/secrets/GITHUB_TOKEN`), falling back to `TREFOIL_SECRET_GITHUB_TOKEN`
in its environment (or `env_file`). A missing required secret rejects the run
with 422 and `missing_secrets: [...]` before anything starts. A brick's `mcps` are streamable-HTTP MCP
servers the harness connects to; `credentials` are HTTP headers where
`${NAME}` expands from the container env. See `bricks/code/github` and
`examples/github`.
An MCP entry with `sigv4: {service, region}` has its requests SigV4-signed
with the container's AWS credentials instead; `cloud:aws` uses it for the
AWS MCP Server (`examples/aws`).

## Skills

Bricks can carry Agent Skills: folders with a `SKILL.md` (front matter
`name`, `description`) plus any files it refers to. Skills ship as zips of
that folder, listed under `skills:` with `source: !Ref file://skills/<name>.zip`:
the CLI packager (and the server, for shared bricks) inlines the zip as a
`data:application/zip;base64,...` string, and `build.Compose` unpacks it into
the build context (the shallowest SKILL.md marks the folder; `__MACOSX/` and
dot files are skipped). Edit one by unzipping, changing and
`zip -rX <name>.zip <name>`. Other sources: `file://some/folder` (packages as
`{relative path: contents}`, text files only; a shared brick also picks up
`skills/<name>/SKILL.md` folders automatically) or `https://...zip` (fetched
at build time by `harness/skills.py`). `name:` overrides the front matter. Skills are installed at `/trefoil/skills/<name>/`; the harness lists
them in the system prompt and adds a `Skill` tool (`name`, optional `file`)
that returns the SKILL.md or another file in the folder. Scripts in a skill
need a tool such as `core:os` Bash to run. See `bricks/docs/release-notes`,
`examples/humanizer` (a skill from a GitHub archive zip)
and `examples/release-notes`.

## Local bricks and split configs

Bricks named without a `<namespace>:` prefix are local: defined in the
config's top-level `bricks:` map, inline or as `!Ref file://bricks/x/config.yml`
(refs in it resolve against its folder, so it can hold `tools.py`, layers
and skill zips like a shared brick). Any config value can be split into its
own file with `!Ref` (an agent, `defaults`, a brick). See `examples/local`.

## Brick tests

A shared brick can have a `tests.py` next to its `config.yml`.
`make test-bricks` (`api/cmd/bricktest`) composes an agent using only that
brick via `build.Compose`, adds the tests and `api/cmd/bricktest/runner.py`,
builds the image (`trefoil-bricktest-<ns>-<name>`) and runs the tests in a
container, so they check the real layers and tool code. Tests are plain
`test_*` functions taking fixtures by name: `tools` (call a tool as
`tools.Write(path=..., content=...)`, loaded by the harness as in a run) and
`tmp` (a fresh directory). Raise `unittest.SkipTest` to skip, e.g. when a
secret is missing; declared secrets are passed in from `NAME` or
`TREFOIL_SECRET_NAME` in the environment. `-v` shows the docker build output.
See `bricks/core/os/tests.py`.
