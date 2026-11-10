# Security policy

memory-sharing-mcp holds API keys and gives agents access to private memory, so
security reports are welcome and taken seriously.

## Reporting a vulnerability

Report privately through GitHub:
[Report a vulnerability](https://github.com/pixie-sh/memory-sharing-mcp/security/advisories/new).
Do not open a public issue, pull request, or discussion for a vulnerability.

Please include:

- the version (`memory-sharing-mcp version`) and operating system
- what an attacker can do, and what they need first (a key, local access, a
  network position)
- steps or a minimal proof of concept
- any memory content, keys, or paths from your own setup removed or replaced

You should get a first reply within 7 days. Once a fix is released, the
advisory is published with credit to you, unless you prefer otherwise.

## Supported versions

Only the latest release gets security fixes.

## Scope

The service is designed for one user's agents on one machine. Its security
model is described under [Security](README.md#security) in the README. In
short, these are in scope:

- reaching the service without a valid key, or doing more than a key's role
  allows
- a revoked key that keeps working longer than a few seconds
- requests from a browser page or DNS rebinding reaching the service
- secrets or memory content appearing in logs, process arguments, URLs, or
  error messages
- a write being replayed or applied twice
- files created with permissions that let other users read keys or the store

These are out of scope, as documented limitations:

- a process running as the same user, which can read key files and the
  database directly
- tools that open the store directly instead of going through the service
- remote setups through a reverse proxy, until that setup is documented and
  supported
- vulnerabilities in Mnemosyne or other providers themselves; report those to
  their projects
