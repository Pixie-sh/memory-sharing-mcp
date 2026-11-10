# memory-sharing-mcp

Share one agent memory store between several AI agents (Pi, Claude Code,
OpenCode, Codex, and any other MCP client) through one authenticated MCP and
REST service.

One process, `http serve`, owns the store. Every agent talks to it, either over
Streamable HTTP MCP or through a small stdio adapter (`mcp serve`). No agent
opens the database directly, so they all go through one pinned runtime, one set
of API keys, and one place that records who wrote what.

Memory stores are plugged in as **providers**. The first provider is
[Mnemosyne](https://github.com/AxDSan/mnemosyne); see
[Adding a provider](#adding-a-provider).

> Status: local (loopback) use works and is tested with Mnemosyne, including the
> macOS service. A [mem0](https://github.com/mem0ai/mem0) provider is planned.
> The Linux systemd service is new and not yet tested on a real machine.
> Not yet done: documented remote access through a reverse proxy.

## Quick start

From nothing to agents with shared memory. From a checkout, `make setup` walks
through all of it: it asks for the provider (only Mnemosyne is available today),
new or existing store, and the Mnemosyne interpreter, offering to install it
with `uv`; then it writes the config, creates the store if new, installs the
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

**mem0.** Not available yet. A mem0 provider is planned. It will take a
`"mem0"` section pointing at an existing mem0 setup (its vector store, history
database, and models) or create a new one with `store init`.

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
- **Not retried by MCP clients.** On MCP, every operation failure is a tool
  error on HTTP 200, because some clients retry 408/429/5xx automatically and
  could replay a write. Only a missing or invalid key (401) or a rejected
  `Host` header (403) fails at the HTTP level, before any operation runs.

## Requirements

- Go 1.26 or later, to build.
- macOS or Linux. Windows is not supported.
- For the Mnemosyne provider: an installation of `mnemosyne-memory`, for example
  `uv tool install mnemosyne-memory==3.15.1`. The version tested is 3.15.1.

## Install

Download a prebuilt binary for macOS or Linux (amd64 or arm64) from
[Releases](https://github.com/pixie-sh/memory-sharing-mcp/releases), check it
against `checksums.txt`, and unpack it:

```bash
v=0.1.0 os=darwin arch=arm64   # or linux / amd64
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
| `provider` | `mnemosyne` | Which memory provider to use. |
| `mnemosyne.data_dir` | required | Directory containing `mnemosyne.db`. `http serve` refuses to start if the file is missing, rather than silently creating an empty store; create one with `store init`. |
| `mnemosyne.python` | required | Interpreter of the `mnemosyne-memory` environment. |
| `mnemosyne.session` | `default` | Mnemosyne session to read and write. Pi uses `default`. |
| `mnemosyne.version` | `3.15.1` | Exact `mnemosyne-memory` version required. Raising it means running an untested version. |
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
memory-sharing-mcp mcp serve     # stdio adapter for one agent
memory-sharing-mcp version
```

`install` takes `--bin-dir DIR` (default `~/.local/bin`) and the global
`--config PATH`, which the service keeps using. It checks that the config and
store exist first. The service is `sh.pixie.memory-sharing-mcp`.

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
`AGENT`, or leave it out to choose from a menu:

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
claude mcp add --scope user memory -- memory-sharing-mcp mcp serve
```

**Pi**, with [pi-mcp-adapter](https://www.npmjs.com/package/pi-mcp-adapter), in
`~/.pi/agent/mcp-adapter.json`. `directTools` exposes the tools under their own
names:

```json
{
  "mcpServers": {
    "memory": { "command": "memory-sharing-mcp", "args": ["mcp", "serve"], "directTools": true }
  }
}
```

**Codex**, in `~/.codex/config.toml`:

```toml
[mcp_servers.memory]
command = "memory-sharing-mcp"
args = ["mcp", "serve"]
```

**OpenCode**, in `~/.config/opencode/opencode.json`:

```json
{
  "mcp": {
    "memory": { "type": "local", "command": ["memory-sharing-mcp", "mcp", "serve"], "enabled": true }
  }
}
```

**Any other MCP client**: a stdio server with command `memory-sharing-mcp` and
arguments `mcp serve`. Clients that support Streamable HTTP with headers can
instead use `http://127.0.0.1:8790/mcp` with `Authorization: Bearer <key>`,
but the key is then stored in plain text in that client's config.

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
- `http serve` must be running. The adapter waits up to 30 seconds for it at
  startup, so agents launched at login do not race the service. If the service
  goes away later, tool calls fail with a connection error and nothing is saved;
  the agent can retry once the service is up.
- Restart the agent after changing its MCP config.

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
  access is meant to go through a reverse proxy or tunnel you configure
  (documentation pending).
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
   for errors that leave it usable, and any other error when it is gone.
2. Add a config section and a `case` in `cmd/memory-sharing-mcp/providers.go`
   that returns its starter and the store path the owner lock is keyed on.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md) for how to contribute.

```bash
go test -race ./...
scripts/smoke.sh <mnemosyne python> <copy of mnemosyne.db>   # end to end, on a temporary copy
make install && make logs                                     # deploy a checkout to the local service
```

To release, push a `vX.Y.Z` tag. The release workflow runs the tests, then
[GoReleaser](https://goreleaser.com) (`.goreleaser.yaml`) builds the binaries,
archives, and `checksums.txt` and publishes them as a GitHub release.

The Mnemosyne tests against a real installation run when `MSM_TEST_PYTHON` and
`MSM_TEST_DB` point at the interpreter and a database copy. Never use a live
store as a test fixture.

## Credits

- [Mnemosyne](https://github.com/AxDSan/mnemosyne) by Abdias J (MIT), the
  memory engine behind the first provider.
- [mem0](https://github.com/mem0ai/mem0), whose memory format the result
  contract follows.
- The [Model Context Protocol](https://modelcontextprotocol.io), created by
  [Anthropic](https://www.anthropic.com), and its
  [official Go SDK](https://github.com/modelcontextprotocol/go-sdk).
- Built with [Claude Code](https://claude.com/claude-code) by Anthropic.

## License

[MIT](LICENSE)
