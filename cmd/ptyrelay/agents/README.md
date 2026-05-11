# Embedded agent binaries

When `ptyrelay` is built with `-tags embedagents`, this directory is
embedded into the resulting binary via `//go:embed`. Each file is one
cross-compiled `ptyrelay-agent` for an `<os>-<arch>` target — the layout
that `bootstrap.EmbedProvider` expects.

Populate this directory before building:

```sh
scripts/build-agents.sh              # writes dist/agents/<os>-<arch>
cp dist/agents/* cmd/ptyrelay/agents/
go build -tags embedagents ./cmd/ptyrelay
```

The default build (no `embedagents` tag) ignores this directory
entirely, keeping the CLI binary small (~6 MB) for users who bootstrap
via `--from-url` or `--provider-dir`.

This directory is gitignored except for this README and a sentinel so
embed has something to point at on a fresh clone.
