# Reference

## Install and verify

Binaries for Linux and macOS (amd64, arm64) and a container image for linux/amd64 and
linux/arm64 are published with each [release](https://github.com/morhaf-ko/zerobill/releases).
Each carries a GitHub build attestation:

```sh
sha256sum --ignore-missing -c checksums.txt      # macOS: shasum -a 256 --ignore-missing -c checksums.txt
gh attestation verify zerobill_<version>_<os>_<arch>.tar.gz --repo morhaf-ko/zerobill \
  --signer-workflow morhaf-ko/zerobill/.github/workflows/release.yml
gh attestation verify oci://ghcr.io/morhaf-ko/zerobill:<version> --repo morhaf-ko/zerobill \
  --signer-workflow morhaf-ko/zerobill/.github/workflows/release.yml
```

Or build it: `go install github.com/morhaf-ko/zerobill/cmd/zerobill@latest`.

## Commands

```text
zerobill init --owner OWNER [--out zerobill.yaml] [--force]
zerobill doctor --config zerobill.yaml
zerobill status --config zerobill.yaml [--state zerobill-state.json] [--overrides ...]
zerobill plan --config zerobill.yaml --observed observed.yaml
zerobill observe --config zerobill.yaml [--state zerobill-state.json] [--out observed.yaml]
zerobill watch --config zerobill.yaml [--state ...] [--applied ...] [--overrides ...] [--interval 5m] [--count 0]
zerobill disable PROVIDER [--for 72h]
zerobill enable PROVIDER
zerobill version
```

Every command except `plan`, `disable` and `enable` reads `GITHUB_TOKEN`. Only `watch`
in `mode: switch` writes to GitHub, and it writes nothing in an interval where any
collector failed. `observe` fails when any collector fails; `watch` logs the error and
skips that provider.

- `init` lists the owner's private repositories with workflows, reads their `runs-on`
  lines, variables and registered runners, and writes a `dry-run` config plus
  `compose.yaml` if missing. It never overwrites a config without `--force`.
- `doctor` checks each permission with one read, the runners for each self-hosted
  label, and every `runs-on` line in `repositories`, and prints a pre-filled token link
  when a permission is missing. It exits non-zero on a failed check.
- `status` prints minutes used, the limit before the reserve, what is left, the
  month-end projection at the current pace, each route's choice and the savings
  estimate. It does not apply `min_hold`, so `watch` can lag its choice.

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
3. A provider disabled with `zerobill disable` is skipped.
4. When nothing qualifies, the route's `fallback` provider is selected and jobs queue.

A route's label stays applied for at least `min_hold` (default `15m`) before moving to
another usable provider, so paced providers swap at most once per hold and a runner that
flaps online does not bounce every queued job. Moving off a provider that went down or
reached its reserve happens on the next interval.

## Collectors

`observe` and `watch` read the config's `owner`, an organization or user, and only send
GET requests:

| Provider | Collector |
| --- | --- |
| `kind: self-hosted` | Online when any runner with the label is online. Queue age is the oldest queued job on the label in `repositories`. Minutes served this month come from jobs that ran on the label in all of the owner's repositories and feed the savings estimate. |
| `kind: metered`, label `blacksmith-*` | Job durations on `blacksmith-*` labels in runs created this month (UTC) in all of the owner's repositories, rounded up per job and scaled by vCPU/2. Jobs that never reached a runner count zero. Completed jobs are cached in `--state`; the first run of a month reads every run and can take minutes. |
| `kind: metered`, label `ubuntu-*` | `Actions Linux` minutes from the billing usage report for this month. |

A user owner must be the token's user, since only they can list their private
repositories. The savings estimate prices self-hosted minutes at GitHub's Linux 2-core
rate of $0.006 a minute; a self-hosted minute served on a larger machine saves more.

## Switching

In `mode: switch`, `watch` reads every route variable from every repository in
`repositories`, and creates or updates only those that differ from the route's chosen
label, so a manual edit is corrected on the next interval. Each route's applied label,
the stalls of on-demand providers and moved runs are kept in `--applied` (default
`zerobill-applied.json`).

`runs-on` is read when a job is queued, so a job already waiting on a self-hosted label
keeps it after the variable changes. Once every variable holds its new label, `watch`
cancels runs with a job queued on a self-hosted label that no route chose and whose
provider is down, saturated or disabled. On the next interval it reruns their failed
and cancelled jobs, which read the new label. A run is moved only when every queued
job is on that label, no job has started and no finished job failed or was cancelled,
so nothing else is stopped or repeated. Each run moves at most once. Jobs queued on the
`fallback` label while nothing has free capacity stay queued.

Each log line lists every route's decision with the variables it changed under
`written`, and the runs it moved under `cancelled` and `rerun`.

## Overrides

`zerobill disable PROVIDER [--for 72h]` writes `--overrides` (default
`zerobill-overrides.json`); a running `watch` reads it on its next interval. Without
`--for` the provider stays disabled until `zerobill enable PROVIDER`. In switch mode,
runs queued on a disabled self-hosted label are moved like runs on a stalled one. Pass
`disable` and `enable` the same `--overrides` path as `watch`; the container image and
Compose example share one working directory, so the defaults match there.

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

## Testing switch mode

Use a throwaway private repository you can delete, so no real CI is touched. It costs
about one GitHub-hosted minute.

1. Add `.github/workflows/probe.yml`:

   ```yaml
   name: probe
   on: workflow_dispatch
   permissions: {}
   jobs:
     probe:
       runs-on: ${{ vars.CI_RUNNER }}
       timeout-minutes: 5
       steps:
         - run: echo "ran on $RUNNER_NAME"
   ```

2. Configure a self-hosted label that no runner serves, and a short wait:

   ```yaml
   mode: switch
   min_hold: 1m
   owner: your-org
   repositories: [your-org/zerobill-test]
   fallback: ghost
   providers:
     - {name: ghost, label: zerobill-ghost, kind: self-hosted, max_queue_wait: 1m, on_demand: true}
     - {name: github, label: ubuntu-24.04, kind: metered, monthly_minutes: 2000, reserve_minutes: 200}
   routes:
     - {name: ci, variables: [CI_RUNNER], providers: [ghost, github]}
   ```

3. Run `zerobill watch --config zerobill.yaml --interval 30s`. The first line creates
   `CI_RUNNER=zerobill-ghost`.
4. Run `gh workflow run probe.yml -R your-org/zerobill-test`. The job queues on the
   ghost label.
5. After about a minute, a line shows `CI_RUNNER` written as `ubuntu-24.04` and the run
   under `cancelled`. The next line shows it under `rerun`, and attempt 2 succeeds on a
   GitHub-hosted runner. Check with `gh run view <id> -R your-org/zerobill-test --attempt 2`.
6. Stop `watch` and delete the repository.
