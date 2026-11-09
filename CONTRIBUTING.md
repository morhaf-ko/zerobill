# Contributing

Open an issue before a large change, so the approach can be agreed first. Small fixes can go straight to a pull request.

- Keep `internal/policy` pure: no I/O, no clock reads.
- Add a test with each behaviour change. Collector and writer tests serve recorded GitHub responses through `httptest`.
- Run `go vet ./...` and `go test -race ./...` before pushing. CI also checks workflow syntax, automation security and real release-archive contents.
- Keep the README focused on setup and value. Put one end-to-end workflow in `docs/getting-started.md`; keep config and failure contracts in `docs/reference.md`. Test installation instructions against an extracted release archive.
- Keep examples free of tokens, real repository names and logs from a live installation.

Renovate proposes dependency updates on Mondays after a five-day release age, and majors wait for approval on its dependency dashboard. Nothing merges automatically, because the repository has no required status checks.

This repository is public, so its `CI_RUNNER` and `RELEASE_RUNNER` variables point at GitHub-hosted runners and it is never listed in a zerobill config. Both variables must exist; a workflow with an unset runner variable fails visibly.

zerobill chooses between free capacity you already have. Changes that create accounts or organizations to collect more free minutes are out of scope.
