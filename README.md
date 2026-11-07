# zerobill

zerobill keeps private-repository CI on GitHub Actions inside free capacity. It runs
jobs on your own hardware when that hardware is online, spills to free hosted minutes
when it is not, and queues instead of spending money when every free tier is used.

It routes by changing repository variables such as `CI_RUNNER`, which workflows read
in `runs-on: ${{ vars.CI_RUNNER }}`. Workflows stay unchanged, and no extra routing job
runs: GitHub rounds every job up to a whole minute, so a per-workflow router job would
spend the minutes zerobill exists to save.

## Status

Milestone 2, switching. `zerobill plan` reads a policy and a provider snapshot and prints
the label it would choose. `zerobill observe` and `zerobill watch` collect live state
from GitHub and log decisions. With `mode: switch`, `watch` also writes the chosen label
to the configured repository variables. Rerunning jobs stuck on an offline label comes
next; see [the design](docs/design.md).

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

## Try it

```sh
go run ./cmd/zerobill plan --config examples/zerobill.yaml --observed examples/observed.yaml
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
export GITHUB_TOKEN=$(gh auth token)
go run ./cmd/zerobill observe --config zerobill.yaml --out observed.yaml
go run ./cmd/zerobill watch --config zerobill.yaml --interval 5m
```

## Switching

Set `mode: switch` to let `watch` write. On each interval it reads every variable in
`variables` from every repository in `repositories`, and creates or updates only those
that differ from the chosen label, so a manual edit is corrected on the next interval.
An interval in which any collector failed writes nothing. The last applied label and
its time are kept in `--applied` (default `zerobill-applied.json`) for the hold.

Writing needs the `repo` scope on a classic token, or the Variables repository
permission with write access on a fine-grained token, in addition to the read access
above. Each log line lists the variables it changed under `written`.

## Scope

zerobill uses free tiers you already have. It does not create accounts or organizations
to collect more free minutes; that breaks provider terms.

## License

[MIT](LICENSE)
