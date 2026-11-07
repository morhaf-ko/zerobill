# Security

Report vulnerabilities through [GitHub private vulnerability reporting](https://github.com/morhaf-ko/zerobill/security/advisories/new). Do not put tokens, private repository names or run logs from your own installation in a public issue. Use public issues for defects whose reproduction contains no private data.

zerobill acts with your GitHub CLI login, or with `GITHUB_TOKEN` when set. With `run --apply` it writes the `CI_RUNNER` and `BOT_RUNNER` repository variables and cancels and reruns workflow runs. To narrow what it can do, give it a fine-grained token with only the permissions in the [reference](docs/reference.md#access).
