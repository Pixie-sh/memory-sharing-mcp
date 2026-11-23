# memory-sharing-mcp

Serve **one memory to many agents**. Pi, Claude Code, OpenCode, Codex, and any
other MCP client read and write the same memory store through one
authenticated MCP and REST service, on one machine or
[across machines](#across-machines).

One process, `http serve`, owns the store. Every agent talks to it, either over
Streamable HTTP MCP or through a small stdio adapter (`mcp serve`). No agent
opens the database directly, so they all go through one pinned runtime, one set
of API keys, and one place that records who wrote what.

Memory stores are plugged in as **providers**. Two are included:
[Mnemosyne](https://github.com/AxDSan/mnemosyne) and
[mem0](https://github.com/mem0ai/mem0). [Choose a provider](#choose-a-provider)
compares them; [Adding a provider](#adding-a-provider) covers more.

> Status: local (loopback) use works and is tested with Mnemosyne, including the
> macOS service. The mem0 provider is new: tested against mem0ai 2.2.1 with a
> local Qdrant store, not yet in long-term use.
> The Linux systemd service has run on one Ubuntu 26.04 container, including
> across a reboot.
> Access from other machines goes through an SSH tunnel or a TLS reverse proxy
> you run ([Across machines](#across-machines)); both have been run on one
> setup each.

## Quick start

From nothing to agents with shared memory. From a checkout, `make setup` walks
through all of it: it asks for the provider (Mnemosyne or mem0), its Python
interpreter, offering to install it with `uv`, and a new or existing store; then it writes the config, creates the store if new, installs the
service, and runs `make gen-config` for your first agent. An existing config is
kept. By hand, steps 1–5 happen once per machine.

```bash
# 1. Install the gateway and a memory provider (Mnemosyne).
go install github.com/pixie-sh/memory-sharing-mcp/cmd/memory-sharing-mcp@latest
uv tool install mnemosyne-memory==3.15.1

# 2. Write ~/.config/memory-sharing-mcp/config.json (see Configure):
mkdir -p ~/.config/memory-sharing-mcp
cat > ~/.config/memory-sharing-mcp/config.json <<'JSON'
{
  "provider": "mnemosyne",
  "mnemosyne": {
    "data_dir": "~/.local/share/memory-sharing-mcp/mnemosyne",
    "python": "~/.local/share/uv/tools/mnemosyne-memory/bin/python"
  }
}
JSON

# 3. Create an empty store, or point data_dir at an existing one instead
#    (see Choose a store). Refuses if a store already exists.
memory-sharing-mcp store init

# 4. Create a key for this machine's agents. Prints the saved key file's path;
#    put that path in the config as "credential_file".
memory-sharing-mcp mcp keygen --name laptop

# 5. Install: copies the binary to ~/.local/bin and starts `http serve` as a
#    macOS LaunchAgent or Linux systemd user unit that runs at login and
#    restarts if it stops.
memory-sharing-mcp install
```

6. Connect your agents: see [Agents setup](#agents-setup).

## Choose a provider

The gateway serves one store, and the provider decides what kind. Both give
agents the same five operations and the same result format, so you can decide
on how you want memory stored and what you are willing to run.

| | [Mnemosyne](https://github.com/AxDSan/mnemosyne) | [mem0](https://github.com/mem0ai/mem0) |
| --- | --- | --- |
| Pick it when | You want one local file and nothing else to run | You already use mem0, or want your own embedding model or vector database |
| Storage | One SQLite file, `mnemosyne.db` | A vector store you configure (Qdrant and others), plus `history.db`, a change log |
| Models | Comes with its install | An embedder and an LLM you choose, hosted or local |
| Write | Stored as given, entities extracted | Stored as given. mem0's own LLM fact extraction is turned off |
| Recall | Vector similarity, full-text match, and importance | Vector similarity |
| Consolidation | `sleep` merges older working memories into episodic summaries | None; `sleep` returns an error |
| Forget | Deletes, or hides memories already consolidated | Deletes |
| Scope | One session; the API key is recorded as author | One `user_id`; the API key is recorded as `agent_id` |
| Tested | 3.15.1, in daily use | 2.2.1 with local Qdrant, new |

```text
              remember(text)                       recall(query)

Mnemosyne     text ─► embed + index ─► working     query ─► vector + full-text + importance
                                       memory                     │
                                         │ sleep                  ▼
                                         ▼                  ranked memories
                                    episodic memory
                              [ one SQLite file: text, vectors, full-text index ]

mem0          text ─► embed ─► vector store        query ─► embed ─► vector search
                               (Qdrant, ...)                      │
                                                                  ▼
                                                            ranked memories
                              [ vector store you configure ] + [ history.db change log ]
```

The Mnemosyne column describes its 3.15.1 release as the gateway uses it; its
own documentation is the reference for the internals.

**Start with Mnemosyne**

1. Install [uv](https://docs.astral.sh/uv/getting-started/installation/), then
   `uv tool install mnemosyne-memory==3.15.1`
   ([PyPI](https://pypi.org/project/mnemosyne-memory/),
   [source and documentation](https://github.com/AxDSan/mnemosyne)).
2. Follow the [Quick start](#quick-start) from step 2.

**Start with mem0**

1. Install [uv](https://docs.astral.sh/uv/getting-started/installation/), then
   create an environment with `mem0ai`
   ([PyPI](https://pypi.org/project/mem0ai/),
   [open-source overview](https://docs.mem0.ai/open-source/overview)):

   ```bash
   uv venv ~/.local/share/memory-sharing-mcp/mem0-env
   uv pip install --python ~/.local/share/memory-sharing-mcp/mem0-env/bin/python mem0ai==2.2.1
   ```

2. Choose an [embedder](https://docs.mem0.ai/components/embedders/overview)
   and a [vector store](https://docs.mem0.ai/components/vectordbs/overview),
   and add to the same environment whatever they need. For example, local
   models through [Ollama](https://ollama.com/download) need its Python
   package (`uv pip install --python <that python> ollama`) and the Ollama app
   running. [Qdrant](https://qdrant.tech/documentation/) with a local `path`
   needs nothing more.
3. Write the config as in [Choose a store](#choose-a-store), run
   `memory-sharing-mcp store init`, and continue the
   [Quick start](#quick-start) from step 4.

`make setup` does either path for you. `store init`, `install`, and
`http serve` first check that the configured interpreter exists and has the
provider's package at the required version, and print the install command
when it does not. A mem0 config that cannot be loaded (a missing model
package, an unreachable model server, no vector store location) stops
startup with mem0's own error.

## Choose a store

The gateway serves exactly one store, chosen by `provider` and that provider's
section in the config file.

**An existing Mnemosyne store.** Set `mnemosyne.data_dir` to the directory that
holds its `mnemosyne.db`. For example, Pi's Mnemosyne extension uses
`~/.pi/agent/mnemosyne`. Then turn off every agent's direct access to that store
(such as Pi's extension or a `mnemosyne mcp` server), so only the gateway opens
it. To move a store instead, copy it while nothing writes to it, with SQLite's
backup command rather than `cp`, which can miss data still in the write-ahead log:

```bash
sqlite3 ~/.pi/agent/mnemosyne/mnemosyne.db ".backup '/new/place/mnemosyne.db'"
```

**A new Mnemosyne store.** Set `mnemosyne.data_dir` to an empty or missing
directory and run `memory-sharing-mcp store init`.

**mem0.** Set `provider` to `mem0`, and give mem0's own configuration (its
models and its vector store) under `mem0.config`:

```json
{
  "provider": "mem0",
  "mem0": {
    "data_dir": "~/.local/share/memory-sharing-mcp/mem0",
    "python": "~/.local/share/memory-sharing-mcp/mem0-env/bin/python",
    "config": {
      "llm": {"provider": "ollama", "config": {"model": "llama3.1"}},
      "embedder": {"provider": "ollama", "config": {"model": "nomic-embed-text"}},
      "vector_store": {"provider": "qdrant", "config": {
        "path": "~/.local/share/memory-sharing-mcp/mem0/qdrant",
        "embedding_model_dims": 768
      }}
    }
  }
}
```

- `mem0.config` is passed to mem0 unchanged; see
  [mem0's documentation](https://docs.mem0.ai) for the providers and their
  fields. `embedding_model_dims` must match the
  embedder. If it holds an API key, keep the config file private (`chmod 600`).
- mem0 needs an `llm` as well as an `embedder` to start, though the gateway
  never calls the LLM: memories are stored as given, one per `remember`, not
  rewritten into extracted facts.
- Where memories are stored is always stated in `mem0.config.vector_store`:
  the gateway refuses to start without it rather than fall back to mem0's
  default, a Qdrant store under `/tmp`. For Qdrant, give a `path` (absolute or
  starting with `~/`), a `host` and `port`, or a `url` and `api_key`. The
  gateway keeps mem0's `history.db` in `data_dir`. For a new store, run
  `memory-sharing-mcp store init`.
- With Qdrant, `http serve` also refuses to start when the configured location
  has no collection, instead of letting mem0 create an empty one after a typo
  or a moved directory. Other vector stores do not have this check yet.
- mem0's process gets a minimal environment. Put what its models need in
  `mem0.env`, for example `{"OLLAMA_HOST": "http://10.0.0.5:11434"}`, a
  proxy, or a model cache directory.
- For an existing mem0 setup, point `data_dir` at the directory holding its
  `history.db` and put its vector store in `mem0.config`. Memories are read
  and written under one mem0 `user_id` (`mem0.user_id`); the API key that wrote
  a memory is recorded as its `agent_id`.
- mem0's usage telemetry is turned off.
- `sleep` is not supported, and `forget` deletes outright.

Changing the store means editing the config and restarting the service
(below). Agents need no change, because they only talk to the gateway.

## How it works

```text
agents ── HTTP MCP ──────────────┐
agents ── stdio ── mcp serve ────┤
scripts ── REST ─────────────────┤
                                 ▼
                http serve (loopback, API keys)
                                 │
              gateway: validate, authorize, queue
                                 │
                 one provider process at a time
                                 │
                           memory store
```

- **One owner.** `http serve` takes an exclusive lock on the store, so a second
  owner refuses to start.
- **One call at a time.** Operations reach the provider strictly one after
  another. Mnemosyne's recall updates the store, so even reads are serialized.
- **No replays.** If a write's outcome is unknown (timeout, crash, lost
  connection), the call is reported as `indeterminate`. It is never retried
  automatically. A crashed provider is restarted after the old process has been
  reaped, with backoff and a cap. When the cap is reached, `http serve` exits
  with an error so the service manager starts a fresh process, instead of
  answering `unavailable` forever.
- **Any machine, one store.** Agents on the same machine connect to loopback.
  Agents elsewhere reach the same service through a tunnel or proxy; see
  [Across machines](#across-machines).
- **Not retried by MCP clients.** On MCP, every operation failure is a tool
  error on HTTP 200, because some clients retry 408/429/5xx automatically and
  could replay a write. Only a missing or invalid key (401) or a rejected
  `Host` header (403) fails at the HTTP level, before any operation runs.

## Requirements

- Go 1.26 or later, to build.
- macOS or Linux. Windows is not supported.
- For the Mnemosyne provider: an installation of `mnemosyne-memory`, for example
  `uv tool install mnemosyne-memory==3.15.1`. The version tested is 3.15.1.
- For the mem0 provider: an environment with `mem0ai` and whatever its
  configured models need. The version tested is 2.2.1. See
  [Choose a provider](#choose-a-provider).

## Install

Download a prebuilt binary for macOS or Linux (amd64 or arm64) from
[Releases](https://github.com/pixie-sh/memory-sharing-mcp/releases), check it
against `checksums.txt`, and unpack it:

```bash
v=0.0.1 os=darwin arch=arm64   # or linux / amd64
curl -LO https://github.com/pixie-sh/memory-sharing-mcp/releases/download/v$v/memory-sharing-mcp_${v}_${os}_${arch}.tar.gz
curl -LO https://github.com/pixie-sh/memory-sharing-mcp/releases/download/v$v/checksums.txt
shasum -a 256 --ignore-missing -c checksums.txt
tar xzf memory-sharing-mcp_${v}_${os}_${arch}.tar.gz memory-sharing-mcp
```

The binaries are not signed. If macOS refuses to open one that was
downloaded with a browser, clear the quarantine flag:
`xattr -d com.apple.quarantine memory-sharing-mcp`.

Or build it with Go:

```bash
go install github.com/pixie-sh/memory-sharing-mcp/cmd/memory-sharing-mcp@latest
```

Then run `memory-sharing-mcp install` to put it on `PATH` and run it as a
service (see [Run](#run)).

From a checkout, the Makefile does both and stamps the binary with the git
version (`make build VERSION=v0.2.0` to choose one):

```bash
make build     # ./memory-sharing-mcp, version from git describe
make setup     # guided first-time setup (see Quick start)
make install   # build, copy to ~/.local/bin, and (re)start the service
make gen-config # connect an agent: its own key and config (see Agents setup)
make logs      # follow the service log
make readiness # service state, /readyz, and the installed version
```

The Makefile assumes macOS. On Linux, set `OS=Linux` for every target, for
example `OS=Linux make install`.

## Configure

The config file is `~/.config/memory-sharing-mcp/config.json`. Override it
with `--config PATH` or `$MSM_CONFIG`. Unknown fields are rejected. Paths must
be absolute or start with `~/`.

```json
{
  "provider": "mnemosyne",
  "mnemosyne": {
    "data_dir": "~/.pi/agent/mnemosyne",
    "python": "~/.local/share/uv/tools/mnemosyne-memory/bin/python"
  }
}
```

| Field | Default | Meaning |
| --- | --- | --- |
| `listen` | `127.0.0.1:8790` | Loopback address for `http serve`. Other addresses are refused. |
| `provider` | `mnemosyne` | Which memory provider to use: `mnemosyne` or `mem0`. |
| `mnemosyne.data_dir` | required | Directory containing `mnemosyne.db`. `http serve` refuses to start if the file is missing, rather than silently creating an empty store; create one with `store init`. |
| `mnemosyne.python` | required | Interpreter of the `mnemosyne-memory` environment. |
| `mnemosyne.session` | `default` | Mnemosyne session to read and write. Pi uses `default`. |
| `mnemosyne.version` | `3.15.1` | Exact `mnemosyne-memory` version required. Raising it means running an untested version. |
| `mem0.data_dir` | required | Directory containing mem0's `history.db`. As with Mnemosyne, `http serve` refuses a missing store. |
| `mem0.python` | required | Interpreter of the `mem0ai` environment. |
| `mem0.user_id` | `default` | mem0 `user_id` to read and write. No whitespace. |
| `mem0.version` | `2.2.1` | Exact `mem0ai` version required. Raising it means running an untested version. |
| `mem0.config` | required | mem0's own configuration (`llm`, `embedder`, `vector_store`), passed through. `vector_store` must say where memories are stored. See [Choose a store](#choose-a-store). |
| `mem0.env` | none | Extra environment variables for mem0's process, as an object of strings. |
| `state_dir` | `~/.local/state/memory-sharing-mcp` | Key registry and owner lock. |
| `registry` | `<state_dir>/keys.json` | Key registry (hashes only). |
| `credentials_dir` | `~/.config/memory-sharing-mcp/credentials` | Where `mcp keygen` saves new keys. |
| `allow_forget`, `allow_sleep` | `false` | Enable delete and consolidation for maintenance keys. |
| `provider_log` | `false` | Pass provider diagnostics to stderr, including the traceback of each failed operation. Off by default because they may quote memory content. |
| `endpoint` | `http://<listen>/mcp` | Where `mcp serve` connects: https, or http on loopback. |
| `credential_file` | none | Key file `mcp serve` reads. `$MSM_API_KEY` overrides it. |

## API keys

```bash
memory-sharing-mcp mcp keygen --name laptop-agents             # saves a 0600 key file, prints its path
memory-sharing-mcp mcp keygen --name ci --role read-only --print   # prints only the secret
memory-sharing-mcp mcp keys list
memory-sharing-mcp mcp keys revoke <key-id>
```

| Operation | read-only | read-write (default) | maintenance |
| --- | --- | --- | --- |
| recall, stats | yes | yes | yes |
| remember | no | yes | yes |
| forget | no | no | only with `allow_forget` |
| sleep (consolidate) | no | no | only with `allow_sleep` |

- The registry stores only SHA-256 hashes. A secret cannot be printed again; to
  rotate, create a new key, update clients, then revoke the old one.
- Revocation applies to the running service within about two seconds, or at once
  on `SIGHUP`.
- New memories record the key ID as their author (`author_type = "api-key"`).
  Memory content is never changed.
- Key commands edit the owner-only registry file directly. Key management is
  never exposed over the network.

## Run

```bash
memory-sharing-mcp install       # copy to ~/.local/bin, run http serve as a LaunchAgent or systemd user unit
memory-sharing-mcp uninstall     # remove the service and binary; keeps config, store, keys
memory-sharing-mcp store init    # create an empty store for the configured provider
memory-sharing-mcp http serve    # owner service in the foreground; logs JSON to stderr
memory-sharing-mcp mcp serve [--caller NAME]  # stdio adapter for one agent; NAME labels its calls in the log
memory-sharing-mcp version
```

`install` takes `--bin-dir DIR` (default `~/.local/bin`) and the global
`--config PATH`, which the service keeps using. It checks that the config and
store exist and that the provider is installed first. The service is `sh.pixie.memory-sharing-mcp`.

On macOS it is a LaunchAgent that logs to `~/Library/Logs/memory-sharing-mcp.log`:

```bash
launchctl kickstart -k gui/$(id -u)/sh.pixie.memory-sharing-mcp   # restart, e.g. after a config change
launchctl print gui/$(id -u)/sh.pixie.memory-sharing-mcp          # status
curl -s http://127.0.0.1:8790/readyz                               # 200 when ready
```

On Linux it is a systemd user unit, `~/.config/systemd/user/sh.pixie.memory-sharing-mcp.service`,
that logs to `~/.local/state/memory-sharing-mcp/memory-sharing-mcp.log`. It
starts when you log in. To start it at boot without a login, run
`loginctl enable-linger $USER` once.

```bash
systemctl --user restart sh.pixie.memory-sharing-mcp   # restart, e.g. after a config change
systemctl --user status sh.pixie.memory-sharing-mcp    # status
```

To upgrade, build or `go install` the new version and run its `install` again
(`make install` from a checkout); it replaces the copy in `~/.local/bin` and
restarts the service. Agents stay connected across the restart.

## Agents setup

Each agent starts the stdio adapter, `memory-sharing-mcp mcp serve`, which reads
the key from `credential_file` in the config file and forwards to `http serve`.

The quickest way is `make gen-config` from a checkout. It checks the
installation, creates a key for one agent, writes that agent's config file to
`~/.config/memory-sharing-mcp/agents/<agent>.json`, and then registers it
(Claude Code, after asking) or prints the snippet to add. Name the agent with
`AGENT`, or leave it out to choose from a menu. The agent name is also passed as
`--caller`, so the service log shows which agent made each call:

```bash
AGENT=claude make gen-config     # also: opencode, pi, codex, or any other name
make gen-config                  # asks which agent
```

Running it again for the same agent reuses its key. The sections below show
the same setup by hand.
Register it once, globally, in each agent you use. Agents launched from a GUI or
a service may not see your shell's `PATH`, so use the absolute path from
`command -v memory-sharing-mcp` where the examples say `memory-sharing-mcp`.

**Claude Code**, for all projects:

```bash
claude mcp add --scope user memory -- memory-sharing-mcp mcp serve --caller claude-code
```

**Pi**, with [pi-mcp-adapter](https://www.npmjs.com/package/pi-mcp-adapter), in
`~/.pi/agent/mcp-adapter.json`. `directTools` exposes the tools under their own
names:

```json
{
  "mcpServers": {
    "memory": { "command": "memory-sharing-mcp", "args": ["mcp", "serve", "--caller", "pi"], "directTools": true }
  }
}
```

**Codex**, in `~/.codex/config.toml`:

```toml
[mcp_servers.memory]
command = "memory-sharing-mcp"
args = ["mcp", "serve", "--caller", "codex"]
```

**OpenCode**, in `~/.config/opencode/opencode.json`:

```json
{
  "mcp": {
    "memory": { "type": "local", "command": ["memory-sharing-mcp", "mcp", "serve", "--caller", "opencode"], "enabled": true }
  }
}
```

**Any other MCP client**: a stdio server with command `memory-sharing-mcp` and
arguments `mcp serve --caller <nickname>`. Clients that support Streamable HTTP
with headers can instead use `http://127.0.0.1:8790/mcp` with
`Authorization: Bearer <key>` and, optionally, `X-Caller: <nickname>`, but the
key is then stored in plain text in that client's config.

The Pi, Claude Code, and OpenCode setups are in daily use; Codex follows its
documented format and is not yet verified.

For example, on 2026-11-10, Claude Code, OpenCode, and Pi each saved a test
memory within the same two seconds. The service log shows three `remember`
calls, each with outcome `ok`, and a recall from any of the three agents then
listed all three entries:

| ID | Source | Saved at |
| --- | --- | --- |
| `mnemosyne:873f19227ff2b13f` | `pi` | 16:23:12 |
| `mnemosyne:e1304704a8c8d0ba` | `ocrobin` | 16:23:11 |
| `mnemosyne:0f4b1f2c581f0d44` | `claude-code` | 16:23:10 |

**Tools.** `memory_recall`, `memory_remember`, `memory_stats`, `memory_forget`,
`memory_sleep`. They take the same fields and return the same results as the
REST API below. Each description tells the model when to use the tool. For
consistent use, add a few lines to the agent's global instructions
(`~/.claude/CLAUDE.md`, `~/.pi/agent/AGENTS.md`, `~/.codex/AGENTS.md`, …):

```markdown
## Memory

Before substantial work, recall relevant context with `memory_recall`. Save
explicit requests, durable preferences, confirmed decisions, and reusable
project gotchas with `memory_remember`. Keep entries concise; never save secrets.
```

**Checklist**

- Turn off the agent's own memory integration for the same store (for example
  Pi's Mnemosyne extension), so the gateway is the only process that opens it.
- One key per machine is enough. To tell agents apart in `agent_id`, give each
  agent its own key and its own small config file that sets `credential_file`,
  and add `--config <file>` before `mcp serve` in that agent's entry.
- To see in the service log which agent made each call while they share a key,
  add `--caller <nickname>` after `mcp serve` (or set `MSM_CALLER`). Each
  `call` log line then carries `caller=<nickname>`. HTTP clients can send the
  same nickname in an `X-Caller` header. It is self-reported and only labels
  log lines; the key still decides what a caller may do. Without one, calls
  still work and log `caller=""`, so you can add nicknames one agent at a time.
- When several profiles of one agent share a server entry (for example a Claude
  Code launcher next to plain `claude`, both using the user-scope `memory`
  server), let the launcher export `MSM_CALLER` and give the entry a fallback:

  ```bash
  claude mcp add --scope user memory -- /bin/sh -c \
    'exec memory-sharing-mcp mcp serve --caller "${MSM_CALLER:-claude-code}"'
  ```
- `http serve` must be running. The adapter waits up to 30 seconds for it at
  startup, so agents launched at login do not race the service. If the service
  goes away later, tool calls fail with a connection error and nothing is saved;
  the agent can retry once the service is up.
- Restart the agent after changing its MCP config.

## Across machines

The point of the gateway is one memory for all your agents, wherever they run:
a laptop, a workstation, a CI runner, a server. The store and `http serve`
live on one machine. Agents on other machines connect to it with their own
API key. The service itself listens on loopback only and does not terminate
TLS, so the network path is yours to provide. Two ways:

**SSH tunnel.** Nothing to configure on the server. On the other machine:

```bash
ssh -N -L 8790:127.0.0.1:8790 user@memory-host
```

Agents there then use `http://127.0.0.1:8790` as if the service were local.

**TLS reverse proxy.** Put a proxy on the server in front of
`127.0.0.1:8790`. It must send a loopback `Host` header upstream, because the
service rejects any other. With
[Caddy](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy):

```text
memory.example.com {
	reverse_proxy 127.0.0.1:8790 {
		header_up Host {upstream_hostport}
	}
}
```

Either way, on each remote machine:

1. Install the `memory-sharing-mcp` binary. It needs no provider or store there.
2. On the server, create a key for that machine
   (`memory-sharing-mcp mcp keygen --name ci-runner`) and copy the key file
   over a secure channel, with mode `0600`.
3. Write its config with only the client side, and register the agents as in
   [Agents setup](#agents-setup):

   ```json
   {
     "endpoint": "https://memory.example.com/mcp",
     "credential_file": "~/.config/memory-sharing-mcp/credentials/key_....key"
   }
   ```

`endpoint` must be `https`, or `http` on a loopback address (the tunnel).
Every request carries the key, and revoking it on the server cuts that
machine off within about two seconds. Anyone who can reach the proxy can try
keys, so limit who can reach it (a private network, a VPN, or the proxy's own
access rules).

## REST

`POST /v1/{recall,remember,stats,forget,sleep}` with a JSON body and
`Authorization: Bearer <key>`:

```bash
curl -s -H "Authorization: Bearer $KEY" -d '{"query":"deploy steps","top_k":5}' \
  http://127.0.0.1:8790/v1/recall
```

| Body | Fields |
| --- | --- |
| recall | `query` (required, ≤ 4096 bytes), `top_k` (1–50, default 5) |
| remember | `content` (required, ≤ 32 KiB), `source` (≤ 64 bytes; default: key name, or `gateway` for an unnamed key), `importance` (0–1, default 0.5) |
| forget | `id`, as returned by recall or remember |
| stats, sleep | none |

Results are the same whichever store is behind the gateway. They follow
[mem0](https://github.com/mem0ai/mem0)'s memory format, so code written for
mem0 results reads them unchanged:

```json
// recall: best match first
{"results": [{"id": "mnemosyne:1f0959a0323a393b", "memory": "...",
              "provider": "mnemosyne", "created_at": "2026-11-08T11:39:37",
              "agent_id": "key_...", "metadata": {"source": "pi", "importance": 0.9}}]}
// remember
{"results": [{"id": "mnemosyne:9c1e...", "memory": "...", "event": "ADD", "provider": "mnemosyne"}]}
// forget: deleted, or hidden (kept but never recalled) where the store
// cannot delete it, such as Mnemosyne memories consolidated by sleep;
// both false when the id is not found
{"deleted": true, "hidden": false}
// stats, sleep: each provider's own report, keyed by provider
{"providers": {"mnemosyne": {...}}}
```

- IDs carry their provider as a prefix.
- `agent_id` is the API key that wrote the memory, where the store records it.
- Store-specific fields go in `metadata`.
- Fields a store does not have are omitted. There is no `score` yet, because
  scores from different stores are not comparable.

Errors are `{"error": "<class>", "message": "..."}`:

| Status | Class |
| --- | --- |
| 400 | `validation` |
| 401 | `unauthorized` |
| 403 | `forbidden` |
| 413 | `too_large` |
| 422 | `provider` |
| 429 | `busy` |
| 502 | `indeterminate` |
| 503 | `unavailable` |

`indeterminate` means a write may have been applied. Check before retrying.
`/healthz` and `/readyz` need no key and report nothing else.

## Security

To report a vulnerability, see [SECURITY.md](SECURITY.md).

- The service listens on loopback only and does not terminate TLS. Remote
  access goes through a tunnel or reverse proxy you configure; see
  [Across machines](#across-machines).
- Every request is authenticated, including each call on an MCP session.
  Requests with a non-loopback `Host` header are rejected (DNS rebinding).
- Secrets never appear in logs, process arguments, or URLs. Logs contain no
  memory content.
- API keys do not protect against a process running as the same user, which can
  read key files and the database directly.
- The owner lock only coordinates `memory-sharing-mcp` instances. Other tools
  that open the store directly bypass it, so turn off agents' built-in direct
  access when you switch them to this service.

## Adding a provider

A provider implements `provider.Provider` (`internal/provider/provider.go`):
one `Call(ctx, op, args)` per operation and `Close`. It maps `recall`,
`remember`, `stats`, `forget`, and `sleep` to its store, returning recall
results as `provider.Memory` values with its own IDs. The gateway does all
validation, authorization, queueing, restarts, and ID prefixing. To add one:

1. Add a package under `internal/provider/<name>`. Return `*provider.OpError`
   for errors that leave it usable, and any other error when it is gone. For a
   store with a Python library, write only the script: `internal/provider/pyproc`
   runs it as a child process and handles the protocol, timeouts, and kills
   (see `internal/provider/mnemosyne`).
2. Add a config section and a `case` in `cmd/memory-sharing-mcp/providers.go`
   that returns its starter and the store path the owner lock is keyed on.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md) for how to contribute.

```bash
go test -race ./...
scripts/smoke.sh <mnemosyne python> <copy of mnemosyne.db>   # end to end, on a temporary copy
make install && make logs                                     # deploy a checkout to the local service
```

To release, move the `Unreleased` entries in [CHANGELOG.md](CHANGELOG.md)
under the new version, commit, and push a `vX.Y.Z` tag. The release workflow runs the tests,
[GoReleaser](https://goreleaser.com) (`.goreleaser.yaml`) builds the binaries,
archives, and `checksums.txt`, and `gh release create --generate-notes`
publishes them as a GitHub release with notes from the merged pull requests
and commits since the previous tag. A tag with a suffix, such as
`v0.2.0-rc.1`, becomes a pre-release.

The Mnemosyne tests against a real installation run when `MSM_TEST_PYTHON` and
`MSM_TEST_DB` point at the interpreter and a database copy. Never use a live
store as a test fixture. The mem0 tests run when `MSM_TEST_MEM0_PYTHON` points
at the interpreter of a `mem0ai` environment; they use temporary stores and a
local stand-in for the embedding model, so they need no API key or network.

## Credits

- [Mnemosyne](https://github.com/AxDSan/mnemosyne) by Abdias J (MIT), the
  memory engine behind the first provider.
- [mem0](https://github.com/mem0ai/mem0) (Apache-2.0), the engine behind the
  second provider, whose memory format the result contract follows.
- The [Model Context Protocol](https://modelcontextprotocol.io), created by
  [Anthropic](https://www.anthropic.com), and its
  [official Go SDK](https://github.com/modelcontextprotocol/go-sdk).
- Built with [Claude Code](https://claude.com/claude-code) by Anthropic.

## License

[MIT](LICENSE)
