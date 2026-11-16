"""Trefoil harness: runs one agent task inside the agent's image.

Reads the agent spec from $TREFOIL_AGENT (written by the API at build time),
loads its tools from /trefoil/tools, takes the task from stdin, and runs a
Claude tool-use loop until the model stops calling tools. The final answer
goes to stdout; progress goes to stderr.

stdin is either JSON ({"task": "..."}) or plain text. An agent with an
input_schema takes a JSON object matching it instead; one with outputs
(output_schema) answers with JSON matching them.

With provider "bedrock" the harness talks to Claude on Amazon Bedrock, using
AWS credentials from the environment: current models through the Messages
API endpoint (Mantle), older ones (Sonnet 4.6, Opus 4.6 and before) through
InvokeModel with the region's cross-region inference profile. Bedrock has
no server tools, server-side fallbacks or task budgets.

MCP servers (agent.json "mcps") are reached over streamable HTTP. Their tools
are offered to the model as <server>__<tool> and called by the harness, so
they work with every provider. Credentials are sent as HTTP headers, with
${NAME} replaced by the environment variable NAME (secrets arrive that way).
A server with "sigv4" ({service, region}) gets every request signed with the
container's AWS credentials, as the AWS MCP Server requires.

Agent Skills installed under /trefoil/skills (see skills.py) are listed in
the system prompt by name and description; the model loads one with the
built-in Skill tool, which returns its SKILL.md or another file in its folder.

Progress events go to stderr as single lines, "x-trefoil-event: <type> <json>",
next to the human-readable [harness] lines; the server parses them into the
run's timeline. Types: start, model, tool_call, tool_result, server_tool,
done, limit and error. Every payload has "ts" (Unix seconds); tool_call and
tool_result share "id". Long strings in payloads are clipped.

Limits come from agent.json, overridden by $TREFOIL_LIMITS (set by the server
from the current config): max_turns, token_budget, max_cost_usd. When one is
reached the harness stops with exit code 3. timeout_seconds is enforced by the
server, which kills the container.
"""

import asyncio
import concurrent.futures
import importlib.util
import json
import os
import re
import sys
import threading
import time
import traceback
from types import SimpleNamespace

import anthropic

import skills

DEFAULT_MODEL = "claude-opus-5-5"
DEFAULT_MAX_TOKENS = 16000
DEFAULT_MAX_TURNS = 50
EXIT_LIMIT = 3

# USD per million tokens: (input, output, cache read). Cache writes are
# billed at 1.25x input. Used for max_cost_usd, so treat costs as estimates.
PRICES = {
    "claude-fable-5-1": (10.00, 50.00, 0.25),
    "claude-fable-5": (10.00, 50.00, 1.00),
    "claude-opus-5-5": (4.00, 20.00, 0.20),
    "claude-opus-5": (5.00, 25.00, 0.50),
    "claude-opus-4-8": (5.00, 25.00, 0.50),
    "claude-sonnet-5-5": (2.00, 10.00, 0.20),
    "claude-sonnet-5": (2.00, 10.00, 0.20),  # after sonnet-5-5: matched by prefix
    "claude-sonnet-4-6": (3.00, 15.00, 0.30),
    "claude-haiku-5-5": (0.10, 0.50, 0.01),
    # Older models (Bedrock InvokeModel); specific prefixes before general ones.
    "claude-opus-4-7": (5.00, 25.00, 0.50),
    "claude-opus-4-6": (5.00, 25.00, 0.50),
    "claude-opus-4-5": (5.00, 25.00, 0.50),
    "claude-opus-4-1": (15.00, 75.00, 1.50),
    "claude-sonnet-4-5": (3.00, 15.00, 0.30),
    "claude-sonnet-4": (3.00, 15.00, 0.30),
    "claude-haiku-4-5": (1.00, 5.00, 0.10),
}
WEB_SEARCH_USD = 10.00 / 1000

# Models that accept a task budget, which lets the model pace itself
# against token_budget instead of being cut off.
TASK_BUDGET_MODELS = ("claude-opus-5-5", "claude-opus-5", "claude-fable-5", "claude-sonnet-5-5",
                      "claude-haiku-5-5", "claude-opus-4-8", "claude-opus-4-7")
MIN_TASK_BUDGET = 20000

# Anthropic-hosted tool versions (dynamic filtering; Claude 4.6+ models).
# A module can pin another version with `type:`.
SERVER_TOOL_TYPES = {"web_search": "web_search_20260318", "web_fetch": "web_fetch_20260318"}

# Models that support server-side refusal fallbacks.
FALLBACK_MODELS = ("claude-fable-5-1", "claude-opus-5-5", "claude-opus-5")

BEDROCK_PREFIX = "anthropic."

# Models served by Bedrock's Messages API endpoint (Mantle). Older models
# only exist on the InvokeModel API, through a cross-region inference profile.
MANTLE_MODELS = ("claude-fable-5-1", "claude-fable-5", "claude-opus-5-5", "claude-opus-5",
                 "claude-opus-4-8", "claude-opus-4-7", "claude-sonnet-5-5", "claude-sonnet-5",
                 "claude-haiku-5-5", "claude-haiku-4-5")

# Bedrock's versioned IDs for older models, as list-foundation-models
# reports them.
BEDROCK_IDS = {
    "claude-opus-4-6": "claude-opus-4-6-v1",
    "claude-opus-4-5": "claude-opus-4-5-20251101-v1:0",
    "claude-opus-4-1": "claude-opus-4-1-20250805-v1:0",
    "claude-sonnet-4-5": "claude-sonnet-4-5-20250929-v1:0",
    "claude-sonnet-4": "claude-sonnet-4-20250514-v1:0",
}

# Inference profile prefixes ("eu.anthropic.claude-...") by region prefix.
PROFILE_GEOS = {"us": "us", "ca": "us", "eu": "eu", "ap": "apac"}
PROFILE_PREFIXES = ("us.", "eu.", "apac.", "jp.", "au.", "global.")


def base_model(model):
    """"eu.anthropic.claude-sonnet-4-6" -> "claude-sonnet-4-6"."""
    for prefix in PROFILE_PREFIXES:
        model = model.removeprefix(prefix)
    return model.removeprefix(BEDROCK_PREFIX)


def price_for(model):
    # Bedrock pricing differs from the first-party API, so Bedrock costs are
    # rougher estimates still.
    model = base_model(model)
    return next((p for m, p in PRICES.items() if model.startswith(m)), None)


def make_client(spec, model):
    """Returns (client, model ID for that client)."""
    if spec.get("provider") != "bedrock":
        return anthropic.Anthropic(), model
    if spec.get("server_tools"):
        sys.exit("[harness] server tools (web_search, web_fetch) are not available on Bedrock")
    # Region falls back to AWS_REGION / AWS_DEFAULT_REGION.
    region = spec.get("aws_region") or os.environ.get("AWS_REGION") or os.environ.get("AWS_DEFAULT_REGION")

    # A full inference profile ID ("global.anthropic.claude-sonnet-4-6") is
    # used as given, on InvokeModel.
    if model.startswith(PROFILE_PREFIXES):
        return anthropic.AnthropicBedrock(aws_region=region), model
    base = base_model(model)
    if base in MANTLE_MODELS:
        return anthropic.AnthropicBedrockMantle(aws_region=region), BEDROCK_PREFIX + base
    geo = PROFILE_GEOS.get((region or "").split("-")[0])
    if geo is None:
        sys.exit(f"[harness] no inference profile for region {region!r}; set model to a full "
                 f"profile ID such as global.anthropic.{base}")
    return anthropic.AnthropicBedrock(aws_region=region), f"{geo}.{BEDROCK_PREFIX}{BEDROCK_IDS.get(base, base)}"


def log(msg):
    print(f"[harness] {msg}", file=sys.stderr, flush=True)


EVENT_PREFIX = "x-trefoil-event: "
EVENT_MAX_STRING = 2000


def clip(value, limit=EVENT_MAX_STRING):
    """Shortens long strings anywhere in value so event lines stay small."""
    if isinstance(value, str):
        return value if len(value) <= limit else value[:limit] + f"… [{len(value) - limit} more chars]"
    if isinstance(value, dict):
        return {k: clip(v, limit) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [clip(v, limit) for v in value]
    return value


def event(kind, **data):
    """Writes one timeline event for the server to parse."""
    data["ts"] = round(time.time(), 3)
    line = json.dumps(clip(data), default=str, ensure_ascii=False)
    print(f"{EVENT_PREFIX}{kind} {line}", file=sys.stderr, flush=True)


def load_spec():
    with open(os.environ.get("TREFOIL_AGENT", "/trefoil/agent.json")) as f:
        return json.load(f)


def load_limits(spec):
    limits = dict(spec.get("limits") or {})
    if os.environ.get("TREFOIL_LIMITS"):
        limits.update({k: v for k, v in json.loads(os.environ["TREFOIL_LIMITS"]).items() if v})
    limits.setdefault("max_turns", DEFAULT_MAX_TURNS)
    return limits


class Usage:
    """Running token and cost totals across the whole run."""

    def __init__(self):
        self.tokens = 0
        self.cost = 0.0
        self.unpriced = set()

    def add(self, response):
        u = response.usage
        cache_read = u.cache_read_input_tokens or 0
        cache_write = u.cache_creation_input_tokens or 0
        self.tokens += u.input_tokens + cache_read + cache_write + u.output_tokens

        price = price_for(response.model)
        if price is None:
            self.unpriced.add(response.model)
        else:
            inp, out, read = price
            self.cost += (u.input_tokens * inp + cache_write * inp * 1.25 + cache_read * read
                          + u.output_tokens * out) / 1e6
        searches = getattr(u.server_tool_use, "web_search_requests", 0) if u.server_tool_use else 0
        self.cost += (searches or 0) * WEB_SEARCH_USD

    def summary(self):
        note = f" (no price for {', '.join(sorted(self.unpriced))})" if self.unpriced else ""
        return f"tokens={self.tokens} cost≈${self.cost:.4f}{note}"


def check_limits(limits, usage, turn):
    """Returns a reason string if a limit has been reached."""
    if limits.get("token_budget") and usage.tokens >= limits["token_budget"]:
        return f"token_budget={limits['token_budget']} reached ({usage.tokens} tokens)"
    if limits.get("max_cost_usd") and usage.cost >= limits["max_cost_usd"]:
        return f"max_cost_usd={limits['max_cost_usd']} reached (≈${usage.cost:.4f})"
    if turn >= limits["max_turns"]:
        return f"max_turns={limits['max_turns']} reached"
    return None


def stop_at_limit(reason, response, usage):
    # Hand back whatever the model had said so far; the run is incomplete.
    partial = "".join(b.text for b in response.content if b.type == "text")
    if partial:
        print(partial)
    log(f"limit: {reason}; {usage.summary()}")
    event("limit", reason=reason, tokens=usage.tokens, cost_usd=round(usage.cost, 6))
    sys.exit(EXIT_LIMIT)


def load_tools(spec):
    """Returns (API tool definitions, name -> callable)."""
    definitions, handlers = [], {}
    for tool in spec.get("tools", []):
        if tool.get("language") != "python":
            log(f"skipping tool {tool['name']}: unsupported language {tool.get('language')!r}")
            continue
        module_spec = importlib.util.spec_from_file_location(f"tool_{tool['name']}", tool["path"])
        module = importlib.util.module_from_spec(module_spec)
        module_spec.loader.exec_module(module)
        # Tool files define a function named by the tool's entry, or by the
        # tool itself.
        handlers[tool["name"]] = getattr(module, tool.get("entry") or tool["name"])
        definitions.append({
            "name": tool["name"],
            "description": tool.get("description", ""),
            "input_schema": tool.get("input_schema") or {"type": "object"},
        })
    return definitions, handlers


def server_tool_definitions(spec):
    """Anthropic runs these tools itself; the harness only declares them."""
    definitions = []
    for tool in spec.get("server_tools", []):
        definition = dict(tool)
        definition.setdefault("type", SERVER_TOOL_TYPES[tool["name"]])
        definitions.append(definition)
    return definitions


class ToolError(Exception):
    """A tool failed in a way worth reporting to the model without a traceback."""


def expand_env(value):
    """Replaces ${NAME} with the environment variable NAME."""
    def sub(match):
        name = match.group(1)
        if name not in os.environ:
            sys.exit(f"[harness] ${{{name}}} is not set; is the secret configured on the server?")
        return os.environ[name]
    return re.sub(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}", sub, value)


def sigv4_auth(service, region):
    """Returns an httpx.Auth that signs requests with AWS SigV4."""
    import botocore.session
    import httpx
    from botocore.auth import SigV4Auth
    from botocore.awsrequest import AWSRequest

    credentials = botocore.session.get_session().get_credentials()
    if credentials is None:
        sys.exit("[harness] sigv4 MCP server needs AWS credentials; none found in the environment")

    class Auth(httpx.Auth):
        requires_request_body = True

        def auth_flow(self, request):
            headers = dict(request.headers)
            # Not part of the signature on the server side; signing it breaks the match.
            headers.pop("connection", None)
            signed = AWSRequest(method=request.method, url=str(request.url), data=request.content, headers=headers)
            SigV4Auth(credentials, service, region).add_auth(signed)
            request.headers.update(dict(signed.headers))
            yield request

    return Auth()


class MCPServers:
    """Holds sessions with the agent's MCP servers.

    The MCP SDK is async and its connections must be opened and closed by
    the same task, so one task on a background event loop owns them all and
    serves tool calls from a queue.
    """

    def __init__(self, servers):
        self.loop = asyncio.new_event_loop()
        threading.Thread(target=self.loop.run_forever, daemon=True).start()
        ready = concurrent.futures.Future()
        self.queue = None
        asyncio.run_coroutine_threadsafe(self._serve(servers, ready), self.loop)
        # (server name, MCP tool) per tool, as listed by each server.
        self.tools = ready.result(timeout=60)

    async def _serve(self, servers, ready):
        from contextlib import AsyncExitStack

        from mcp import ClientSession
        from mcp.client.streamable_http import streamablehttp_client

        self.queue = asyncio.Queue()
        try:
            async with AsyncExitStack() as stack:
                sessions, tools = {}, []
                for server in servers:
                    headers = {k: expand_env(v) for k, v in (server.get("credentials") or {}).items()}
                    sigv4 = server.get("sigv4")
                    auth = sigv4_auth(sigv4["service"], sigv4["region"]) if sigv4 else None
                    read, write, _ = await stack.enter_async_context(
                        streamablehttp_client(server["url"], headers=headers, auth=auth)
                    )
                    session = await stack.enter_async_context(ClientSession(read, write))
                    await session.initialize()
                    sessions[server["name"]] = session
                    cursor = None
                    while True:
                        page = await session.list_tools(cursor)
                        tools += [(server["name"], t) for t in page.tools]
                        cursor = page.nextCursor
                        if not cursor:
                            break
                ready.set_result(tools)
                while True:
                    server, name, arguments, result = await self.queue.get()
                    try:
                        result.set_result(await sessions[server].call_tool(name, arguments))
                    except Exception as e:
                        result.set_exception(e)
        except BaseException as e:
            if not ready.done():
                ready.set_exception(e)
            raise

    def call(self, server, name, arguments):
        result = concurrent.futures.Future()
        self.loop.call_soon_threadsafe(self.queue.put_nowait, (server, name, arguments, result))
        res = result.result()
        parts = [c.text if c.type == "text" else json.dumps(c.model_dump(mode="json")) for c in res.content]
        output = "\n".join(parts)
        if res.isError:
            raise ToolError(output or f"{name} failed")
        return output


def load_mcp_tools(spec):
    """Returns (API tool definitions, name -> callable) for the agent's MCP servers."""
    if not spec.get("mcps"):
        return [], {}
    try:
        servers = MCPServers(spec["mcps"])
    except Exception as e:
        sys.exit(f"[harness] connecting to MCP servers failed: {e!r}")
    definitions, handlers = [], {}
    for server, tool in servers.tools:
        name = re.sub(r"[^a-zA-Z0-9_-]", "_", f"{server}__{tool.name}")[:128]
        definitions.append({"name": name, "description": tool.description or "", "input_schema": tool.inputSchema})
        handlers[name] = lambda args, s=server, t=tool.name: servers.call(s, t, vars(args))
    log(f"mcp: {len(definitions)} tools from {[s['name'] for s in spec['mcps']]}")
    return definitions, handlers


SKILL_TOOL = "Skill"


def load_skills():
    """Returns (API tool definitions, name -> callable, system prompt section)
    for the skills installed in the image."""
    installed = {s["name"]: s for s in skills.discover()}
    if not installed:
        return [], {}, ""

    def load(args):
        skill = installed.get(args.name)
        if skill is None:
            raise ToolError(f"unknown skill {args.name!r}; available: {', '.join(installed)}")
        rel = getattr(args, "file", None)
        try:
            text = skills.read_file(skill, rel)
        except (OSError, ValueError) as e:
            raise ToolError(str(e))
        if rel:
            return text
        files = skills.listing(skill["path"], skill["path"])
        return f"Skill folder: {skill['path']}\nFiles: {', '.join(files)}\n\n{text}"

    definition = {
        "name": SKILL_TOOL,
        "description": "Loads a skill's instructions (its SKILL.md), or another file from the skill's "
                       "folder when `file` is given. Load a skill before doing a task it covers.",
        "input_schema": {
            "type": "object",
            "properties": {
                "name": {"type": "string", "enum": sorted(installed), "description": "Skill name"},
                "file": {"type": "string", "description": "Path inside the skill folder, e.g. reference.md"},
            },
            "required": ["name"],
        },
    }
    section = ("# Skills\n\nSkills are folders of instructions, scripts and resources for particular tasks. "
               f"When a task matches a skill's description, call the {SKILL_TOOL} tool to load it first "
               "and follow its instructions; files it mentions are relative to its folder.\n\n"
               + "\n".join(f"- {s['name']}: {s['description']}" for s in installed.values()))
    log(f"skills: {sorted(installed)}")
    return [definition], {SKILL_TOOL: load}, section


def read_task(spec):
    raw = sys.stdin.read().strip()
    if not raw:
        sys.exit("[harness] no task on stdin")
    if spec.get("input_schema"):
        # The server has already checked the input against the schema.
        return "Input:\n```json\n" + json.dumps(json.loads(raw), indent=2) + "\n```"
    try:
        data = json.loads(raw)
    except ValueError:
        return raw
    if isinstance(data, dict) and "task" in data:
        return str(data["task"])
    return raw


# Tool results go into every later request, so a huge one (a noisy test run,
# a cat of a minified file) can push the prompt past the model's context
# window. Keep the start and the end, where errors and summaries usually are.
TOOL_OUTPUT_MAX = 30_000


def cap_output(output, limit=TOOL_OUTPUT_MAX):
    if len(output) <= limit:
        return output
    half = limit // 2
    cut = len(output) - 2 * half
    return (output[:half] + f"\n\n[… {cut:,} chars cut from the middle of this output; redirect it to a file "
            "and read or grep the parts you need …]\n\n" + output[-half:])


def summarize_output(output):
    """The text of a tool result for logs and events, with images as placeholders."""
    if isinstance(output, str):
        return output
    return "\n".join(b["text"] if b.get("type") == "text" else f"[{b.get('type')}]" for b in output)


def run_tool(handlers, block):
    handler = handlers.get(block.name)
    if handler is None:
        return f"Error: unknown tool {block.name!r}", True
    try:
        output = handler(SimpleNamespace(**block.input))
        # A list is content blocks (text and images); anything else is text.
        return (output if isinstance(output, list) else str(output)), False
    except ToolError as e:
        return f"Error: {e}", True
    except Exception:
        return f"Error running {block.name}:\n{traceback.format_exc()}", True


def main():
    spec = load_spec()
    tools, handlers = load_tools(spec)
    mcp_tools, mcp_handlers = load_mcp_tools(spec)
    tools += mcp_tools
    handlers.update(mcp_handlers)
    skill_tools, skill_handlers, skills_prompt = load_skills()
    if skill_handlers.keys() & handlers.keys():
        sys.exit(f"[harness] a module tool is named {SKILL_TOOL}, which skills need")
    tools += skill_tools
    handlers.update(skill_handlers)
    tools += server_tool_definitions(spec)

    system = "\n\n".join([p["source"] for p in spec.get("prompts", []) if p.get("role", "system") == "system"]
                         + ([skills_prompt] if skills_prompt else []))
    messages = [{"role": "user", "content": p["source"]} for p in spec.get("prompts", []) if p.get("role") == "user"]
    messages.append({"role": "user", "content": read_task(spec)})

    model = spec.get("model") or DEFAULT_MODEL
    bedrock = spec.get("provider") == "bedrock"
    client, api_model = make_client(spec, model)
    limits = load_limits(spec)
    params = {"model": api_model, "max_tokens": spec.get("max_tokens") or DEFAULT_MAX_TOKENS, "betas": []}
    if system:
        params["system"] = system
    if tools:
        params["tools"] = tools
    if model in FALLBACK_MODELS and not bedrock:
        params["betas"].append("server-side-fallback-2026-06-01")
        params["fallbacks"] = [{"model": "claude-opus-4-8"}]
    if model in TASK_BUDGET_MODELS and not bedrock and limits.get("token_budget", 0) >= MIN_TASK_BUDGET:
        params["betas"].append("task-budgets-2026-03-13")
        params.setdefault("output_config", {})["task_budget"] = {"type": "tokens", "total": limits["token_budget"]}
    if spec.get("output_schema"):
        # The final answer must be JSON matching the agent's outputs.
        params.setdefault("output_config", {})["format"] = {"type": "json_schema", "schema": spec["output_schema"]}
    if not params["betas"]:
        del params["betas"]
    if limits.get("max_cost_usd") and price_for(model) is None:
        sys.exit(f"[harness] no price for model {model}; can't enforce max_cost_usd")

    usage = Usage()
    log(f"agent={spec.get('name')} provider={spec.get('provider') or 'anthropic'} model={api_model} "
        f"tools={sorted(handlers)} "
        f"server_tools={[t['name'] for t in spec.get('server_tools', [])]} limits={limits}")
    event("start", agent=spec.get("name"), provider=spec.get("provider") or "anthropic", model=api_model,
          tools=sorted(handlers), server_tools=[t["name"] for t in spec.get("server_tools", [])])

    turn = 0
    while True:
        turn += 1
        with client.beta.messages.stream(messages=messages, **params) as stream:
            response = stream.get_final_message()
        messages.append({"role": "assistant", "content": response.content})
        usage.add(response)
        event("model", turn=turn, stop_reason=response.stop_reason,
              input_tokens=response.usage.input_tokens, output_tokens=response.usage.output_tokens,
              tokens=usage.tokens, cost_usd=round(usage.cost, 6),
              text="".join(b.text for b in response.content if b.type == "text"))

        if response.stop_reason == "refusal":
            category = response.stop_details.category if response.stop_details else None
            sys.exit(f"[harness] model refused (category: {category})")
        if response.stop_reason == "max_tokens":
            sys.exit(f"[harness] hit max_tokens={params['max_tokens']}; raise defaults.max_tokens")
        for block in response.content:
            if block.type == "server_tool_use":
                log(f"turn {turn}: {block.name}({json.dumps(block.input)[:200]}) [server]")
                event("server_tool", turn=turn, id=block.id, tool=block.name, input=block.input)
        # pause_turn: a server tool hit its iteration limit; re-sending the
        # conversation as-is resumes it (handled after the limit check below).

        tool_uses = [b for b in response.content if b.type == "tool_use"]
        if response.stop_reason != "pause_turn" and not tool_uses:
            print("".join(b.text for b in response.content if b.type == "text"))
            log(f"done in {turn} turns; {usage.summary()}")
            event("done", turns=turn, tokens=usage.tokens, cost_usd=round(usage.cost, 6))
            return

        reason = check_limits(limits, usage, turn)
        if reason:
            stop_at_limit(reason, response, usage)
        if response.stop_reason == "pause_turn":
            continue

        results = []
        for block in tool_uses:
            log(f"turn {turn}: {block.name}({json.dumps(block.input)[:200]})")
            event("tool_call", turn=turn, id=block.id, tool=block.name, input=block.input)
            started = time.monotonic()
            output, is_error = run_tool(handlers, block)
            texts = [output] if isinstance(output, str) else [b for b in output if b.get("type") == "text"]
            for text in texts:
                value = text if isinstance(text, str) else text["text"]
                if len(value) > TOOL_OUTPUT_MAX:
                    log(f"turn {turn}: {block.name} output cut from {len(value):,} to {TOOL_OUTPUT_MAX:,} chars")
                    if isinstance(text, str):
                        output = cap_output(value)
                    else:
                        text["text"] = cap_output(value)
            event("tool_result", turn=turn, id=block.id, tool=block.name, is_error=is_error,
                  ms=round((time.monotonic() - started) * 1000), output=summarize_output(output))
            results.append({"type": "tool_result", "tool_use_id": block.id, "content": output, "is_error": is_error})
        messages.append({"role": "user", "content": results})


if __name__ == "__main__":
    try:
        main()
    except SystemExit as e:
        # sys.exit("[harness] ...") is how the harness gives up; put the
        # reason on the timeline too.
        if isinstance(e.code, str):
            event("error", message=e.code.removeprefix("[harness] "))
        raise
    except BaseException as e:
        event("error", message=f"{type(e).__name__}: {e}")
        raise
