# zerobill

zerobill keeps private-repository CI on GitHub Actions inside free capacity. It runs
jobs on your own hardware when that hardware is online, spills to free hosted minutes
when it is not, and queues instead of spending money when every free tier is used.

It routes by changing repository variables such as `CI_RUNNER`, which workflows read
in `runs-on: ${{ vars.CI_RUNNER }}`. Workflows stay unchanged, and no extra routing job
runs: GitHub rounds every job up to a whole minute, so a per-workflow router job would
spend the minutes zerobill exists to save.

## Status

Milestone 1, dry run. `zerobill plan` reads a policy and a provider snapshot and prints
the label it would choose. Live collectors and variable switching come next; see
[the design](docs/design.md).

## How it decides

Providers are tried in the order you list them:

1. A self-hosted provider is used while it is online and its oldest queued job has
   waited less than `max_queue_wait`.
2. A metered provider is used while its used minutes stay below
   `monthly_minutes - reserve_minutes`. The reserve stops a switch onto a tier that
   would run dry mid-job.
3. When nothing qualifies, the `fallback` provider is selected and jobs queue.

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

## Scope

zerobill uses free tiers you already have. It does not create accounts or organizations
to collect more free minutes; that breaks provider terms.

## License

[MIT](LICENSE)
