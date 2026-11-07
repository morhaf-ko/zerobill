# zerobill

[![CI](https://github.com/morhaf-ko/zerobill/actions/workflows/ci.yml/badge.svg)](https://github.com/morhaf-ko/zerobill/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/morhaf-ko/zerobill)](https://github.com/morhaf-ko/zerobill/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

zerobill keeps private-repository CI on GitHub Actions inside free capacity. It runs
jobs on your own hardware when that hardware is online, spills to free hosted minutes
when it is not, and queues instead of spending money when every free tier is used.

It routes by changing repository variables such as `CI_RUNNER`, which workflows read
in `runs-on: ${{ vars.CI_RUNNER }}`. Workflows stay unchanged, and no extra routing job
runs: GitHub rounds every job up to a whole minute, so a per-workflow router job would
spend the minutes zerobill exists to save.

## Install

Download a binary for Linux or macOS, amd64 or arm64, from the
[latest release](https://github.com/morhaf-ko/zerobill/releases/latest). Each release
publishes `checksums.txt` and a GitHub build attestation:

```sh
sha256sum --ignore-missing -c checksums.txt
gh attestation verify zerobill_*_linux_arm64.tar.gz --repo morhaf-ko/zerobill
```

Or build it with Go:

```sh
go install github.com/morhaf-ko/zerobill/cmd/zerobill@latest
```

## Quick start

1. Point each job at a variable: `runs-on: ${{ vars.CI_RUNNER }}`.
2. Copy [`examples/zerobill.yaml`](examples/zerobill.yaml) and list your owner,
   repositories, variables and providers in priority order.
3. Watch the decisions in dry run for a while. Nothing is written:

   ```sh
   export GITHUB_TOKEN=...   # see Token
   zerobill watch --config zerobill.yaml --interval 5m
   ```

4. Set `mode: switch` and run `watch` on an always-on host, for example with the
   systemd unit in [`examples/zerobill.service`](examples/zerobill.service).

## Status

Milestones 1 and 2 are done: live collectors, dry run, variable switching with a
minimum hold, and moving jobs stuck on an unusable self-hosted label. Monthly reports
come next; see [the design](docs/design.md).

## How it decides

Providers are tried in the order you list them:

1. A self-hosted provider is used while it is online and its oldest queued job has
   waited less than `max_queue_wait`.
2. A metered provider is used while its used minutes stay below
   `monthly_minutes - reserve_minutes`. The reserve stops a switch onto a tier that
   would run dry mid-job.
3. When nothing qualifies, the `fallback` provider is selected and jobs queue.

In switch mode a label stays applied for at least `min_hold` (default `15m`) before
moving back to a higher-priority provider, so a runner that flaps online does not
bounce every queued job. Moving off a provider that went offline, saturated or reached
its reserve happens on the next interval.

## Try a decision

```sh
zerobill plan --config examples/zerobill.yaml --observed examples/observed.yaml
```

```text
mode: dry-run
decision: blacksmith -> blacksmith-2vcpu-ubuntu-2404
reason: 1460 free minutes above reserve (skipped home: offline)
would set CI_RUNNER, RELEASE_RUNNER, LIGHT_RUNNER on your-org/your-app to blacksmith-2vcpu-ubuntu-2404
```

## Live state

`observe` and `watch` read `GITHUB_TOKEN` and the config's `owner`, an organization or
user, and only send GET requests. Each provider's collector follows from its config:

| Provider | Collector |
| --- | --- |
| `kind: self-hosted` | Online when any runner with the label is online. Queue age is the oldest queued job on the label in `repositories`. |
| `kind: metered`, label `blacksmith-*` | Job durations on `blacksmith-*` labels in runs created this month (UTC) in all of the owner's repositories, rounded up per job and scaled by vCPU/2. Completed jobs are cached in `--state`. |
| `kind: metered`, label `ubuntu-*` | `Actions Linux` minutes from the billing usage report for this month. |

The billing report needs organization admin access, or the `user` scope for a user
account. A user owner must also be the token's user, since only they can list their
private repositories. `watch` logs one JSON line per interval; a provider whose collector fails is
reported under `errors` and skipped by the policy.

```sh
zerobill observe --config zerobill.yaml --out observed.yaml
zerobill watch --config zerobill.yaml --interval 5m
```

## Switching

Set `mode: switch` to let `watch` write. On each interval it reads every variable in
`variables` from every repository in `repositories`, and creates or updates only those
that differ from the chosen label, so a manual edit is corrected on the next interval.
An interval in which any collector failed writes nothing. The last applied label and
its time are kept in `--applied` (default `zerobill-applied.json`) for the hold.

`runs-on` is read when a job is queued, so a job already waiting on a self-hosted label
keeps it after the variable changes. Once every variable holds the new label, `watch`
cancels runs with a job queued on a self-hosted label whose provider is offline or
saturated, and on the next interval reruns their failed and cancelled jobs, which read
the new label. A run is moved only when every queued job is on that label, no job has
started and no finished job failed or was cancelled, so nothing else is stopped or
repeated. Each run moves at most once. Jobs queued on the `fallback` label while nothing has
free capacity stay queued.

Each log line lists the variables it changed under `written` and the runs it moved
under `cancelled` and `rerun`.

## Token

`observe` and `watch` read the token from `GITHUB_TOKEN`.

| Need | Classic token | Fine-grained token |
| --- | --- | --- |
| Runners, runs and jobs | `repo` | Repository Actions and Administration: read; organization Self-hosted runners: read |
| GitHub-hosted minutes | `user` for a user owner; organization admin for an organization | Not documented by GitHub; use a classic token |
| Switch mode | `repo` | Repository Variables and Actions: write |

A classic token with `repo` and, for a user owner with a GitHub-hosted provider, `user`
covers everything. With the GitHub CLI: `gh auth refresh -s user` adds the scope.

## Configuration

| Key | Meaning |
| --- | --- |
| `mode` | `dry-run` (default) logs decisions; `switch` lets `watch` write. |
| `min_hold` | Minimum time a label stays before moving back to a higher-priority provider. Default `15m`. |
| `owner` | Organization or user whose runners, billing and repositories are read. |
| `repositories` | `owner/name` list whose variables are written and whose queues are watched. |
| `variables` | Repository variables set to the chosen label. |
| `fallback` | Provider chosen when none has free capacity; jobs queue there. |
| `providers[].kind` | `self-hosted` or `metered`. |
| `providers[].max_queue_wait` | Self-hosted only: queue age at which the provider counts as saturated. |
| `providers[].monthly_minutes`, `reserve_minutes` | Metered only: free minutes per month and the part kept back. |

## Scope

zerobill uses free tiers you already have. It does not create accounts or organizations
to collect more free minutes; that breaks provider terms.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately as described
in [SECURITY.md](SECURITY.md).

## Author

Built and maintained by Morhaf Kourbaj:
[GitHub](https://github.com/morhaf-ko) · [LinkedIn](https://www.linkedin.com/in/mkourbaj/).

## License

[MIT](LICENSE)
