# Reference

## Config

`~/.config/zerobill/config.yaml`, or `$XDG_CONFIG_HOME/zerobill/config.yaml`:

```yaml
owner: your-org
self_hosted_label: home-ci
reserve_percent: 10
```

Run `zerobill init` to create it interactively. The owner is required; self-hosted capacity
is optional and Blacksmith defaults to off. Init comments out optional defaults and leaves
existing configs untouched. It does not sign in to GitHub or change workflows.

| Key | Meaning |
| --- | --- |
| `owner` | Required. The GitHub user or organization whose private repositories zerobill routes. |
| `self_hosted_label` | Optional GitHub Actions label shared by self-hosted machines zerobill may select. Find it in Settings → Actions → Runners. |
| `blacksmith` | `true` to use Blacksmith's free minutes too. |
| `reserve_percent` | Percentage kept back from each free tier. Integer from 0 to 99; defaults to 10 when omitted. Zero uses the full allowance. |
| `check_interval` | Availability and usage polling interval, default `5m`; accepts `1m` to `1h`. Restart the service after config changes. |
| `queue_timeout` | Treat a self-hosted pool as unavailable once a job waits longer, default `10m`; accepts `1m` to `1h`. |
| `alert_remaining_percent` | Warn at this percentage of each full hosted allowance remaining. Default 20; integer 0–99; 0 disables warnings. Choose a value above the reserve for advance notice. |
| `notify` | A URL told whenever a variable's choice changes: an [ntfy](https://ntfy.sh/) topic, a Discord or Slack webhook, or anything that accepts a plain-text POST. Without `--apply` the message ends in `[log only]`, and every restart reports the first choice again, since nothing is saved. Routing-change send failures are logged and not retried. Low-capacity warnings are sent only with `--apply` and complete reads; successful warnings are saved once per provider, owner and UTC month. Failed low-capacity sends retry on the next successful check. Changing the threshold or webhook does not reset a sent warning. |

Containers can set `ZEROBILL_OWNER`, `ZEROBILL_SELF_HOSTED_LABEL`, `ZEROBILL_BLACKSMITH` (`true`),
`ZEROBILL_RESERVE_PERCENT`, `ZEROBILL_ALERT_REMAINING_PERCENT`,
`ZEROBILL_CHECK_INTERVAL`, `ZEROBILL_QUEUE_TIMEOUT` and `ZEROBILL_NOTIFY` instead; when `ZEROBILL_OWNER` is set
the file is not read. The reserve has the same default and range.

Rename old `runner` / `ZEROBILL_RUNNER` settings to `self_hosted_label` /
`ZEROBILL_SELF_HOSTED_LABEL`. Old names fail with an explicit migration message.

State lives in `~/.local/state/zerobill` (`$XDG_STATE_HOME/zerobill`): a cache of this
month's jobs, the last decision per variable, and pauses.

## Access

zerobill uses `GITHUB_TOKEN`, then `GH_TOKEN`, then the stored GitHub CLI login
(`gh auth token`). Status displays the source and authenticated account. Environment
tokens take precedence over the stored login. Reading runners and billing needs `gh auth refresh -s admin:org` for
an organization, or `-s user` for a personal account. A personal account must be the
signed-in user. For a narrower token, see [security](../SECURITY.md).

`status` exits with 1 when reads fail, no private repositories are visible, or no
workflow uses the routing variables. It prints recovery commands and withholds
usage estimates and recommendations. A billing 404 can mean missing access or an
unavailable billing report; adding scopes is not a guaranteed fix.

Blacksmith requires its GitHub App to be installed, but zerobill calls no Blacksmith
API. Its usage estimate comes from GitHub Actions job labels and durations.

## Routing

| Rule | Value |
| --- | --- |
| Repositories | Private repositories whose workflows read `vars.CI_RUNNER` or `vars.BOT_RUNNER`, rescanned hourly |
| `CI_RUNNER` | Usable self-hosted capacity first, otherwise the less-used hosted allowance |
| `BOT_RUNNER` | Optional hosted-first policy, sharing available hosted allowances |
| Free minutes | GitHub: 2,000 on Free, 3,000 on Pro and Team, read from your plan. Blacksmith: 3,000. |
| Reserve | Stop selecting a free tier at its configured limit: allowance × (100 − `reserve_percent`) / 100 |
| Sharing | Among usable free tiers, the one with the smallest share used, so all last the month |
| Nothing free | Jobs wait for your runner, or queue on reserved label `zerobill-wait` without one; never assign this label to a runner |
| Interval | `check_interval`, default 5 minutes |

## Hosted balancing and outages

zerobill compares `used_minutes / usable_allowance`, where usable allowance is the
monthly allowance after the reserve. It selects the lower fraction, retaining a
usable hosted choice for 15 minutes to avoid switching on every check. For example,
900 of 1,800 GitHub minutes and 1,350 of 2,700 Blacksmith minutes are both 50% used.
Ties choose Blacksmith when enabled. There is currently no configurable hosted order.

Proportional sharing is the default recommendation: it keeps capacity available in
both tiers without another setup decision. Blacksmith-first could suit a user who
prefers that provider's performance, but draining one allowance first leaves fewer
free alternatives later. Neither algorithm detects a provider outage by itself.

A GitHub-hosted compute problem might leave Blacksmith usable if Actions scheduling
and GitHub's API still work. An Actions control-plane outage can affect every runner:
Blacksmith [documented a GitHub broker failure](https://status.blacksmith.sh/cmltqllbp0e9u12d4lpxlwgci)
that prevented its provisioned runners from accepting jobs. Self-hosted runners also
[receive assignments from GitHub](https://docs.github.com/en/actions/reference/runners/self-hosted-runners).
Do not treat multiple runner providers as independence from GitHub.

Hosted-provider queue detection and automatic outage failover are not implemented.
Manual `pause github` or `pause blacksmith` excludes that tier on the next check,
when GitHub's API and another free tier are available. It does not migrate jobs
already queued on the hosted tier. A failed API read leaves existing variables
unchanged, so neither automatic routing nor a pause guarantees recovery during an
API outage. Provider status pages are diagnostic context, not proof of whether a
particular job can start.

## How a runner counts as down

With no matching runner registered, zerobill initially assumes an on-demand pool
will create one for a job. Selecting the self-hosted label at this point does not
prove that a machine is online. An online observation and a successful job on that
runner establish availability; a queue stall triggers the rules below.

- A registered runner with your label is offline, or
- a job has waited for the label longer than `queue_timeout` (default 10 minutes). Runners created per job, such as
  GARM or Actions Runner Controller pools, register only when a job arrives, so this is
  how zerobill notices they are gone. A pool that keeps idle runners, such as GARM with
  warm runners, then stays down until one is online again, so an outage costs one wait,
  and is tried again after six hours at most. A pool that never shows idle runners is
  tried again after an hour.
- `zerobill pause runner [duration]` takes it out until `zerobill resume runner`.

A usable hosted choice is held for 15 minutes to reduce switching between hosted tiers.
Returning to usable self-hosted capacity bypasses the hold. Moving off a runner that
is down, or a tier at its reserve, happens on the next successful check. A shorter
`check_interval` increases API use; it does not make GitHub usage reporting real time.

`BOT_RUNNER` is optional hosted-first routing for users who want to leave their own
machines free from bot jobs:

```yaml
runs-on: ${{ github.actor == 'renovate[bot]' && vars.BOT_RUNNER || vars.CI_RUNNER }}
```

Most users should use only `CI_RUNNER`. When hosted capacity is exhausted, both
variables fall back to the configured self-hosted label, even if it is unavailable
or paused, or to `zerobill-wait` without self-hosting. An unmatched label queues jobs
until GitHub's queue timeout; changing a variable does not move already-queued jobs.
zerobill does not automatically rescue jobs queued on `zerobill-wait`. It prevents
new jobs from using a metered tier; it does not pause existing executions.

The free-capacity and outage limits are explained in the
[README](../README.md#routing-and-spending-protection).

## What `run --apply` does

Each interval it sets `CI_RUNNER` and `BOT_RUNNER` in the repositories that read them,
writing only values that differ, so a manual edit is corrected. An interval in which any
read failed writes nothing.

`runs-on` is read when a job is queued, so changing a variable does not move an
already queued job. Once all variables hold the selected labels, zerobill rechecks
the run attempt and cancels runs whose queued jobs still target the unavailable
runner. It leaves running or failed jobs alone and preserves successful jobs.

Recovery uses the existing check interval and persistent state:

1. Request normal cancellation and wait for confirmed completion.
2. After at least five minutes, request force cancellation once only if the same
   attempt still has queued jobs and no job in that attempt has executed. Completed
   successful jobs also prevent force cancellation. Empty or ambiguous job metadata,
   changed attempts and deployment/concurrency waits prevent escalation.
3. After confirmed cancellation, request one rerun of cancelled jobs. Then verify a
   new attempt and its visible jobs' labels against the applied routes before
   reporting `rerun`. Unrelated failed jobs are not retried.

Each request's intent is saved before sending it. Restarts do not repeat uncertain
requests. Force cancellation and rerun verification each have a five-minute wait,
evaluated on the next successful check. An unresolved recovery remains recorded
instead of being automatically cancelled again. Logs show `recovery` with the run
URL, attempt and reason; a configured `notify` hook receives the warning once after
successful delivery. Failed warning delivery retries on the next successful check.
`force_cancel_requested` reports GitHub accepting a request, not completion.

GitHub's force-cancel endpoint bypasses conditions such as `always()` and has no
atomic attempt precondition. Fresh checks reduce but cannot eliminate a race with
external reruns or newly started work. Some GitHub-side orphaned runs cannot be
cleared even by force cancellation; inspect the linked run and follow
[GitHub's troubleshooting guidance](https://docs.github.com/en/actions/how-tos/troubleshoot-workflows#canceling-workflows).
zerobill does not dispatch replacement workflows or bypass concurrency protections.

Keep the state volume across upgrades. Before downgrading an applied controller,
stop it and have the operator inspect pending recovery records: older binaries do
not understand the new request phases. Do not delete the journal to retry recovery.

Without `--apply`, `run` logs the same decisions as JSON lines and writes nothing.

## Running it as a service

Follow [service setup](service.md) for installation, startup, logs, updates and stop
commands. Docker is optional. A native service reads that user's YAML config; the
Compose example uses environment settings and a persistent state volume.

Commands read the config, credential and state on the machine where they execute.
A laptop command does not control an instance on your VPS. Run commands there over
SSH, or use `docker compose exec zerobill zerobill status` and
`docker compose exec zerobill zerobill pause runner 7d` in its Compose directory.
`status` performs fresh reads and reports recommendations; it does not prove the
background controller is running or that a recommendation was applied. Check its
logs for the latest completed check and writes.

Immediate refresh and remote-client profiles are not implemented. A pause is read
on the next check; configuration changes require restarting the controller.

`run --apply --health :8080` optionally serves `/healthz`. It answers `200` while its
loop started or finished a check in the last `max(15m, 3 × check_interval)` and `503`
otherwise. It reports zerobill's loop, not Forge's power state or GitHub access. Read
failures stay in the log; restarting does not repair provider access. Runner health
is read from GitHub, so no direct Forge URL is required.
