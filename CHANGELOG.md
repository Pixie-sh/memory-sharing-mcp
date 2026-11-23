# Changelog

Notable changes to memory-sharing-mcp. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Each GitHub release
also lists the pull requests and commits it contains.

## [Unreleased]

### Added

- Landing page, `docs/index.html`, for GitHub Pages.
- Caller nickname in the service log: `mcp serve --caller NAME` (or
  `MSM_CALLER`), or an `X-Caller` header on HTTP, adds `caller` to each `call`
  log line. It is self-reported and does not affect authorization.
- mem0 provider (`"provider": "mem0"`), tested with `mem0ai` 2.2.1. It takes
  mem0's own configuration under `mem0.config`, stores memories as given
  under one `user_id`, requires the vector store location to be configured,
  and turns mem0's telemetry off. `sleep` is not supported. With Qdrant it
  refuses to start on a location that has no collection. `mem0.env` passes
  extra environment variables to mem0's process.
  `make setup` offers it, and `version mem0` prints the tested version.
- `store init`, `install`, and `http serve` check that the provider's
  interpreter exists and has its package at the required version, and print
  the install command when it does not.
- README: a provider comparison with setup links, and how to reach the service
  from other machines.

### Changed

- The Python child-process runner moved out of the Mnemosyne provider into
  `internal/provider/pyproc`, which both providers use. No behavior change
  for Mnemosyne.

## [0.0.1] - 2026-11-10

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

[Unreleased]: https://github.com/pixie-sh/memory-sharing-mcp/compare/v0.0.1...HEAD
[0.0.1]: https://github.com/pixie-sh/memory-sharing-mcp/releases/tag/v0.0.1
