# Contributing

Thanks for helping. Bug reports, fixes, documentation, and new providers are
all welcome. For a security problem, follow [SECURITY.md](SECURITY.md) instead
of opening an issue.

## Before you start

For anything larger than a small fix, such as a new provider, a new command, or
a change to the API, open an issue first so the approach can be agreed before
you write it.

## Setup

You need Go 1.26 or later, on macOS or Linux. For the end-to-end tests, also
install the Mnemosyne provider:

```bash
uv tool install mnemosyne-memory==3.15.1
```

## Checks

CI runs these on Linux and macOS; run them before opening a pull request:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -race ./...
```

If you change the Mnemosyne provider or the gateway, also run the tests
against a real installation and the end-to-end smoke test, each on a **copy** of
a store:

```bash
MSM_TEST_PYTHON=~/.local/share/uv/tools/mnemosyne-memory/bin/python \
MSM_TEST_DB=/tmp/copy/mnemosyne.db go test ./internal/provider/mnemosyne/
scripts/smoke.sh ~/.local/share/uv/tools/mnemosyne-memory/bin/python /tmp/copy/mnemosyne.db
```

Never point a test at a live store. To try a change on your own machine,
`make install && make logs` deploys the checkout to the local service.

If you change a script in `scripts/`, run `shellcheck` on it.

## Writing changes

- Keep changes small and focused, with the shortest diff that solves the
  problem. Prefer the standard library and existing code over new dependencies
  and new abstractions.
- Add a test for new logic, using the existing test files and `go test`.
- Keep the safety rules in the README's [How it works](README.md#how-it-works)
  and [Security](README.md#security) sections: one owner process, operations
  serialized, writes never replayed, no secrets or memory content in logs.
- Update the README when behavior, commands, or configuration change, and add
  user-visible changes under `Unreleased` in [CHANGELOG.md](CHANGELOG.md).
- New providers follow [Adding a provider](README.md#adding-a-provider).

## Pull requests

Describe what changed and why, and how you verified it: which commands you ran
and on which operating system. Link the issue it addresses, if any.

## Releases

Maintainers release by pushing a `vX.Y.Z` tag; see
[Development](README.md#development).

## License

By contributing, you agree that your contributions are licensed under the
[MIT License](LICENSE).
