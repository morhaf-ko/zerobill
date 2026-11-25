# Reference

## Config

`~/.config/zerobill/config.yaml`, or `$XDG_CONFIG_HOME/zerobill/config.yaml`:

```yaml
owner: your-org
self_hosted_label: home-ci
github_reserve_percent: 5
blacksmith_reserve_percent: 0
```

Run `zerobill init` to create it interactively. The owner is required; self-hosted capacity
is optional and Blacksmith defaults to off. Init comments out optional defaults and leaves
existing configs untouched. It does not sign in to GitHub or change workflows.

| Key | Meaning |
| --- | --- |
| `owner` | Required. The GitHub user or organization whose private repositories zerobill routes. |
| `self_hosted_label` | Optional GitHub Actions label shared by self-hosted machines zerobill may select. Find it in Settings → Actions → Runners. |
| `blacksmith` | `true` to use Blacksmith's free minutes too. |
| `github_reserve_percent` | Percentage kept back from the GitHub Actions allowance. Integer from 0 to 99. Defaults to 5. |
| `blacksmith_reserve_percent` | Percentage kept back from the Blacksmith allowance. Integer from 0 to 99. Defaults to 0, which uses the full allowance. |
| `check_interval` | Availability and usage polling interval, default `5m`; accepts `1m` to `1h`. Restart the service after config changes. |
| `queue_timeout` | Treat a self-hosted pool as unavailable once a job waits longer, default `2m`; accepts `1m` to `1h`. |
| `alert_remaining_percent` | Warn at this percentage of each full hosted allowance remaining. Default 20; integer 0–99; 0 disables warnings. Choose a value above the reserves of enabled providers for advance notice. |
| `notify` | A URL told whenever a variable's choice changes: an [ntfy](https://ntfy.sh/) topic, a Discord or Slack webhook, or anything that accepts a plain-text POST. Without `--apply` the message ends in `[log only]`, and every restart reports the first choice again, since nothing is saved. Routing-change send failures are logged and not retried. Low-capacity warnings are sent only with `--apply` and complete reads; successful warnings are saved once per provider, owner and UTC month. Failed low-capacity sends retry on the next successful check. Changing the threshold or webhook does not reset a sent warning. |

Containers can set `ZEROBILL_OWNER`, `ZEROBILL_SELF_HOSTED_LABEL`, `ZEROBILL_BLACKSMITH` (`true`),
`ZEROBILL_GITHUB_RESERVE_PERCENT`, `ZEROBILL_BLACKSMITH_RESERVE_PERCENT`, `ZEROBILL_ALERT_REMAINING_PERCENT`,
`ZEROBILL_CHECK_INTERVAL`, `ZEROBILL_QUEUE_TIMEOUT` and `ZEROBILL_NOTIFY` instead; when `ZEROBILL_OWNER` is set
the file is not read. Reserve defaults and ranges are the same in both forms.

An explicit legacy `reserve_percent` or `ZEROBILL_RESERVE_PERCENT` still sets the
reserve for both hosted providers. A provider-specific setting overrides it for
that provider. To adopt the new defaults, remove the legacy setting and its
Compose fallback if your copy still defaults it to 10.

Rename old `runner` / `ZEROBILL_RUNNER` settings to `self_hosted_label` /
`ZEROBILL_SELF_HOSTED_LABEL`. Old names fail with an explicit migration message.

State lives in `~/.local/state/zerobill` (`$XDG_STATE_HOME/zerobill`): a cache of this
month's jobs, the latest status snapshot, the last applied decision per variable,
and pauses.

## Access

zerobill uses `GITHUB_TOKEN`, then `GH_TOKEN`, then the stored GitHub CLI login
(`gh auth token`). `status --refresh` displays the source and authenticated account. Environment
tokens take precedence over the stored login. Reading runners and billing needs `gh auth refresh -s admin:org` for
an organization, or `-s user` for a personal account. A personal account must be the
signed-in user. For a narrower token, see [security](../SECURITY.md).

`status --refresh` exits with 1 when reads fail, no private repositories are visible, or no
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
| Reserve | Stop selecting a free tier at its configured limit: allowance × (100 − the provider’s reserve percentage) / 100 |
| Sharing | Among usable free tiers, the one with the smallest share used, so all last the month |
| Nothing free | Jobs wait for your runner, or queue on reserved label `zerobill-wait` without one; never assign this label to a runner |
| Interval | `check_interval`, default 5 minutes |

## Hosted balancing and outages

zerobill compares `used_minutes / usable_allowance`, where usable allowance is the
monthly allowance after the reserve. It selects the lower fraction, retaining a
usable hosted choice for 15 minutes to avoid switching on every check. For example,
950 of 1,900 GitHub minutes and 1,500 of 3,000 Blacksmith minutes are both 50% used.
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

- Matching runners are registered but none are online, or
- a job has waited for the label longer than `queue_timeout` (default 2 minutes). Runners created per job, such as
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
writing differing values. A failed provider, pause, or complete discovery read
blocks variable writes for that check. A partial workflow scan omits repositories
it could not inspect. A failed variable operation is reported for that repository
and route. Pending recoveries are still read and reported in each of these cases.

`runs-on` is evaluated when a job queues. For automatic recovery, zerobill needs a
fresh queued or in-progress run, an unassigned, unstarted job with only the
stalled label, and an eligible hosted replacement confirmed in that job's repository
variable. It reads the workflow at the original commit for `push` and
`workflow_dispatch` and matches the jobs by their static names. Use a single
`${{ vars.CI_RUNNER }}` or `${{ vars.BOT_RUNNER }}` label. Fixed labels, group or
multi-label declarations, conditional expressions, matrix and reusable workflows,
and other events can leave recovery for the operator. Routing variables are still
updated according to the normal policy.

1. Request normal cancellation once. Already successful, skipped, or neutral jobs
   can remain in the attempt. Running, failed, assigned, or ambiguous jobs block
   cancellation.
2. After at least five minutes, request force cancellation once if the same
   attempt, same affected jobs, and same verified route still qualify. GitHub
   waiting, pending, or requested runs remain under GitHub's scheduler.
3. After confirmed cancellation, use GitHub's failed-jobs rerun endpoint once.
   The cancelled jobs and their replacement routes must still match the saved
   plan. A changed route, an exhausted reserve, or an unavailable variable blocks
   the request. GitHub also reschedules dependent jobs, so a declared dependency
   on an affected job leaves recovery to the operator.
4. Track the migrated jobs by name in the next attempt. `rerun` reports that their
   replacement labels were confirmed. `recovery` reports queue or GitHub-side
   waiting, runner assignment, and observed execution. A matching sibling label
   does not verify a migrated job. Missing evidence or pickup after the verification
   window requires inspection of the linked run.

Each request's intent is saved before sending it. Restarts preserve ownership
and deduplicate requests and confirmations. Recovery with uncertain state stays
recorded for operator attention, including old state that lacks job identities.
A configured `notify` hook receives an attention warning once after successful
delivery. `force_cancel_requested` records that GitHub accepted a request.

The JSON log reports workflow discovery coverage and `check_delayed_by` when a
check starts late. Inspect `observed.runner.queue` for runner inventory and queue
observations. Busy runners, offline runners, and an empty inventory have different
signals. Visible runners must carry every requested label, and an organization
inventory cannot establish runner-group access for a repository or workflow.
Missing, future, or stale job observations leave queue age unknown. A short wait
with no registration can be a cold start, and a wait beyond `queue_timeout` can
also indicate a pool outage. Where a runner appears eligible and a job stays
queued, inspect GitHub approvals, concurrency, and runner-group access before
attributing a cause.

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
`status` reads the local snapshot without GitHub credentials. Its capacity table shows each provider’s usage, reserve and remaining free minutes.
The routing table shows the last applied runner and any recommendation supported by
a fresh, complete check. The heading shows the age of the check. After upgrading, an applied controller check records assignments for
the configured owner. A check older than three check intervals is stale. Minutes from a previous
UTC month are unavailable. `status --refresh` performs live reads and adds month-end
projections, setup checks and access details. A successful refresh can update displayed usage while the
applied controller supplies assignments and workflow coverage. Check controller logs for write
errors or recovery details.

A pause is read on the next check. Configuration changes require restarting the
controller.

`run --apply --health :8080` optionally serves `/healthz`. It answers `200` while its
loop started or finished a check in the last `max(15m, 3 × check_interval)` and `503`
otherwise. It reports zerobill's loop, not Forge's power state or GitHub access. Read
failures stay in the log; restarting does not repair provider access. Runner health
is read from GitHub, so no direct Forge URL is required.
