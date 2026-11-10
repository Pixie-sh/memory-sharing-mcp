# Changelog

Notable changes to memory-sharing-mcp. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Each GitHub release
also lists the pull requests and commits it contains.

## [Unreleased]

First release.

### Added

- `http serve`: one owner process for the memory store, on loopback, with
  Streamable HTTP MCP and a REST API (`/v1/recall`, `remember`, `stats`,
  `forget`, `sleep`) and `/healthz` and `/readyz`.
- `mcp serve`: stdio adapter for agents, which waits up to 30 seconds for the
  service at startup.
- Mnemosyne provider, tested with `mnemosyne-memory` 3.15.1, and
  `store init` to create a new store.
- API keys with read-only, read-write, and maintenance roles: `mcp keygen` and
  `mcp keys list|revoke`. Only hashes are stored, and revocation applies within
  about two seconds.
- Results in mem0's memory format, with provider-prefixed IDs and the writing
  key recorded as `agent_id`.
- `install` and `uninstall`: run `http serve` as a macOS LaunchAgent or a Linux
  systemd user unit.
- `make setup` for guided first-time setup, and `make gen-config` to connect
  Claude Code, OpenCode, Pi, Codex, or another agent with its own key.
- Prebuilt binaries for macOS and Linux (amd64, arm64) with checksums.

[Unreleased]: https://github.com/pixie-sh/memory-sharing-mcp/commits/main
