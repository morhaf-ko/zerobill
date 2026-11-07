# zerobill

[![CI](https://github.com/morhaf-ko/zerobill/actions/workflows/ci.yml/badge.svg)](https://github.com/morhaf-ko/zerobill/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/morhaf-ko/zerobill)](https://github.com/morhaf-ko/zerobill/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

zerobill keeps private-repository CI on GitHub Actions inside free capacity. It runs
jobs on your own hardware when that hardware is online, spills to free hosted minutes
when it is not, and queues instead of spending money when every free tier is used.

It routes by changing repository variables such as `CI_RUNNER`, which workflows read
in `runs-on: ${{ vars.CI_RUNNER }}`. No extra routing job runs: GitHub rounds every job
up to a whole minute, so a per-workflow router job would spend the minutes zerobill
exists to save.

- **Routes** give different work different providers: your own pushes go to your
  hardware first, while dependency bots such as Renovate use only the free tiers.
- **Pace balancing** spreads jobs across several free tiers by the share of each one
  already used, so every tier lasts the month.
- **On-demand runners** from GARM or Actions Runner Controller count as down only when
  a job waits too long, not because no runner is registered between jobs.
- **`zerobill disable`** takes a provider out of rotation, for example while you travel.

## Install

Download a binary for Linux or macOS, amd64 or arm64, from the
[latest release](https://github.com/morhaf-ko/zerobill/releases/latest). Each release
publishes `checksums.txt` and a GitHub build attestation:

```sh
sha256sum --ignore-missing -c checksums.txt      # macOS: shasum -a 256 --ignore-missing -c checksums.txt
gh attestation verify zerobill_<version>_<os>_<arch>.tar.gz --repo morhaf-ko/zerobill \
  --signer-workflow morhaf-ko/zerobill/.github/workflows/release.yml
```

Or run the container image for linux/amd64 or linux/arm64, which carries its own
attestation:

```sh
gh attestation verify oci://ghcr.io/morhaf-ko/zerobill:<version> --repo morhaf-ko/zerobill \
  --signer-workflow morhaf-ko/zerobill/.github/workflows/release.yml
```

Or build it with Go:

```sh
go install github.com/morhaf-ko/zerobill/cmd/zerobill@latest
```

## Quick start

1. Point each job at a variable: `runs-on: ${{ vars.CI_RUNNER }}`. To give dependency
   bots their own route, choose the variable by actor:

   ```yaml
   runs-on: ${{ github.actor == 'renovate[bot]' && vars.BOT_RUNNER || vars.CI_RUNNER }}
   ```

2. Copy [`examples/zerobill.yaml`](examples/zerobill.yaml) and list your owner,
   repositories, providers and routes. List only private repositories: on a public
   repository, a pull request from a fork would run on your self-hosted hardware, which
   [GitHub advises against](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/about-self-hosted-runners#self-hosted-runner-security).
3. Run it on an always-on machine such as a Raspberry Pi, NAS or small VPS with
   [`examples/compose.yaml`](examples/compose.yaml):

   ```sh
   docker compose up -d
   docker compose logs -f zerobill   # one JSON decision per interval
   ```

   Without Docker, run `zerobill watch --config zerobill.yaml` under the systemd unit
   in [`examples/zerobill.service`](examples/zerobill.service).
4. Read the decisions in `dry-run`, then set `mode: switch` and restart to let it write.

## Status

Milestones 1 and 2 are done: live collectors, dry run, variable switching with a
minimum hold, moving jobs stuck on an unusable self-hosted label, routes, pace
balancing, on-demand runners and operator overrides. Monthly reports come next; see
[the design](docs/design.md).

## How it decides

Each route tries its providers in the order it lists them:

1. A self-hosted provider is used while its oldest queued job has waited less than
   `max_queue_wait` and a runner with its label is online. With `on_demand: true` no
   online runner is needed; a stall marks it down for `retry_after` (default `1h`) or
   until a runner comes online.
2. A metered provider is used while its used minutes stay below
   `monthly_minutes - reserve_minutes`. The reserve stops a switch onto a tier that
   would run dry mid-job. With `balance: pace`, the route takes the usable metered
   provider that has used the smallest share of those minutes.
   Within `min_hold` the applied provider is kept, so paced providers swap at most
   once per hold.
3. A provider disabled with `zerobill disable` is skipped.
4. When nothing qualifies, the route's `fallback` provider is selected and jobs queue.

A route's label stays applied for at least `min_hold` (default `15m`) before moving to
another usable provider, so a runner that flaps online does not bounce every queued
job. Moving off a provider that went down or reached its reserve happens on the next
interval.

## Try a decision

```sh
zerobill plan --config examples/zerobill.yaml --observed examples/observed.yaml
```

```text
mode: dry-run
route ci: github -> ubuntu-24.04
  reason: lowest share of free minutes used: blacksmith 46%, github 17% (skipped home: queue waited 12m0s)
  would set CI_RUNNER, RELEASE_RUNNER, LIGHT_RUNNER on your-org/your-app to ubuntu-24.04
route bot: github -> ubuntu-24.04
  reason: lowest share of free minutes used: blacksmith 46%, github 17%
  would set BOT_RUNNER on your-org/your-app to ubuntu-24.04
```

## Take a provider out of rotation

```sh
zerobill disable home --for 72h   # until zerobill enable home without --for
zerobill enable home
```

The override is kept in `--overrides` (default `zerobill-overrides.json`), and a running
`watch` reads it on its next interval. In switch mode, runs queued on a disabled
self-hosted label are moved like runs on a stalled one. With Compose, prefix the
commands with `docker compose exec zerobill`.

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

Set `mode: switch` to let `watch` write. On each interval it reads every route variable
from every repository in `repositories`, and creates or updates only those that differ
from the route's chosen label, so a manual edit is corrected on the next interval. An
interval in which any collector failed writes nothing. Each route's applied label and
its time, and the stalls of on-demand providers, are kept in `--applied` (default
`zerobill-applied.json`).

`runs-on` is read when a job is queued, so a job already waiting on a self-hosted label
keeps it after the variable changes. Once every variable holds the new label, `watch`
cancels runs with a job queued on a self-hosted label that no route chose and whose
provider is down, saturated or disabled, and on the next interval reruns their failed and cancelled jobs, which read
the new label. A run is moved only when every queued job is on that label, no job has
started and no finished job failed or was cancelled, so nothing else is stopped or
repeated. Each run moves at most once. Jobs queued on the `fallback` label while nothing has
free capacity stay queued.

Each log line lists every route's decision with the variables it changed under
`written`, and the runs it moved under `cancelled` and `rerun`.

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
| `min_hold` | Minimum time a route's label stays before moving to another usable provider. Default `15m`. |
| `owner` | Organization or user whose runners, billing and repositories are read. |
| `repositories` | `owner/name` list whose variables are written and whose queues are watched. |
| `fallback` | Default provider chosen when nothing in a route has free capacity; jobs queue there. |
| `providers[].kind` | `self-hosted` or `metered`. |
| `providers[].max_queue_wait` | Self-hosted only: queue age at which the provider counts as down or saturated. |
| `providers[].on_demand`, `retry_after` | Self-hosted only: runners are created per job, so only a queue stall marks the provider down, for `retry_after` (default `1h`). |
| `providers[].monthly_minutes`, `reserve_minutes` | Metered only: free minutes per month and the part kept back. |
| `routes[].name`, `variables` | A route and the repository variables it sets. Each variable belongs to one route. |
| `routes[].providers` | Provider names the route may use, in priority order. |
| `routes[].balance` | `order` (default) takes the first usable provider; `pace` spreads metered providers by the share of free minutes used. |
| `routes[].fallback` | Overrides the top-level `fallback` for this route. |
| `variables` | Without `routes`: the variables of a single route over every provider. |

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
