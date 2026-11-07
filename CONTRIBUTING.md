# Contributing

Open an issue before a large change, so the approach can be agreed first. Small fixes can go straight to a pull request.

- Keep `internal/policy` pure: no I/O, no clock reads.
- Add a test with each behaviour change. Collector and writer tests serve recorded GitHub responses through `httptest`.
- Run `go vet ./...` and `go test -race ./...` before pushing. CI also runs actionlint, an offline zizmor scan and `goreleaser check`.
- Keep examples free of tokens, real repository names and logs from a live installation.

zerobill chooses between free capacity you already have. Changes that create accounts or organizations to collect more free minutes are out of scope.
