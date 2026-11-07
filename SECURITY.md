# Security

Report vulnerabilities through [GitHub private vulnerability reporting](https://github.com/morhaf-ko/zerobill/security/advisories/new). Do not put tokens, private repository names or watch logs from your own installation in a public issue. Use public issues for defects whose reproduction contains no private data.

zerobill holds a GitHub token that can write repository variables and cancel and rerun workflow runs. Give it the narrowest token that covers your configured `owner` and `repositories`, plus Actions read access to every repository of the owner when a Blacksmith provider is configured, as listed in the [README](README.md#token).
