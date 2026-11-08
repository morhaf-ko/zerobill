# Reference

## Config

`~/.config/zerobill/config.yaml`, or `$XDG_CONFIG_HOME/zerobill/config.yaml`:

```yaml
owner: your-org
runner: home-ci
reserve_percent: 10
```

Run `zerobill init` to create it interactively. The owner is required; the runner
is optional and Blacksmith defaults to off. Init uses a 10% reserve and leaves
existing configs untouched. It does not sign in to GitHub or change workflows.

| Key | Meaning |
| --- | --- |
| `owner` | Required. The GitHub user or organization whose private repositories zerobill routes. |
| `runner` | The label of your self-hosted runners. Leave it out without one. |
| `blacksmith` | `true` to use Blacksmith's free minutes too. |
| `reserve_percent` | Percentage kept back from each free tier. Integer from 0 to 99; defaults to 10 when omitted. Zero uses the full allowance. |
| `notify` | A URL told whenever a variable's choice changes: an [ntfy](https://ntfy.sh/) topic, a Discord or Slack webhook, or anything that accepts a plain-text POST. Without `--apply` the message ends in `[log only]`, and every restart reports the first choice again, since nothing is saved. A failed send is logged and not retried. |

Containers can set `ZEROBILL_OWNER`, `ZEROBILL_RUNNER`, `ZEROBILL_BLACKSMITH` (`true`),
`ZEROBILL_RESERVE_PERCENT` and `ZEROBILL_NOTIFY` instead; when `ZEROBILL_OWNER` is set
the file is not read. The reserve has the same default and range.

State lives in `~/.local/state/zerobill` (`$XDG_STATE_HOME/zerobill`): a cache of this
month's jobs, the last decision per variable, and pauses.

## Access

zerobill uses `GITHUB_TOKEN` when it is set, and otherwise the GitHub CLI login
(`gh auth token`). Reading runners and billing needs `gh auth refresh -s admin:org` for
an organization, or `-s user` for a personal account. A personal account must be the
signed-in user. For a narrower token, see [security](../SECURITY.md).

## Conventions

| Rule | Value |
| --- | --- |
| Repositories | Private repositories whose workflows read `vars.CI_RUNNER` or `vars.BOT_RUNNER`, rescanned hourly |
| `CI_RUNNER` | Your runner, then Blacksmith, then GitHub-hosted (`ubuntu-latest`) |
| `BOT_RUNNER` | Blacksmith, then GitHub-hosted |
| Free minutes | GitHub: 2,000 on Free, 3,000 on Pro and Team, read from your plan. Blacksmith: 3,000. |
| Reserve | Stop selecting a free tier at its configured limit: allowance × (100 − `reserve_percent`) / 100 |
| Sharing | Among usable free tiers, the one with the smallest share used, so all last the month |
| Nothing free | Jobs wait for your runner, or use GitHub-hosted without one |
| Interval | Every 5 minutes |

## How a runner counts as down

- A registered runner with your label is offline, or
- a job has waited for the label more than 10 minutes. Runners created per job, such as
  GARM or Actions Runner Controller pools, register only when a job arrives, so this is
  how zerobill notices they are gone. A pool that keeps idle runners, such as GARM with
  warm runners, then stays down until one is online again, so an outage costs one wait,
  and is tried again after six hours at most. A pool that never shows idle runners is
  tried again after an hour.
- `zerobill pause runner [duration]` takes it out until `zerobill resume runner`.

A label stays applied for at least 15 minutes while it is still usable, so a flapping
runner does not bounce every queued job. Moving off a runner that is down, or a tier at
its reserve, happens on the next interval.

## What `run --apply` does

Each interval it sets `CI_RUNNER` and `BOT_RUNNER` in the repositories that read them,
writing only values that differ, so a manual edit is corrected. An interval in which any
read failed writes nothing.

`runs-on` is read when a job is queued, so a job already waiting on a down runner keeps
its label. Once every variable holds the new label, zerobill cancels runs whose queued
jobs all target the down runner, and on the next interval reruns their cancelled jobs,
which read the new label. Runs with a started or failed job are left alone, and each run
moves at most once.

Without `--apply`, `run` logs the same decisions as JSON lines and writes nothing.

## Running it as a service

- **Docker:** [`examples/compose.yaml`](../examples/compose.yaml) runs the image
  read-only, without capabilities, as a non-root user. It reads
  `/etc/zerobill/config.yaml` and keeps state in a volume. Containers cannot use your
  GitHub CLI login, so pass `GITHUB_TOKEN`: `GITHUB_TOKEN=$(gh auth token)`, or a
  fine-grained token in `.env` with only the permissions in [security](../SECURITY.md).
- **Linux service:** [`examples/zerobill.service`](../examples/zerobill.service) is a
  systemd user service that uses your GitHub CLI login. Read its decisions with
  `journalctl --user -u zerobill -f`; on systems without per-user journals, use
  `sudo journalctl _SYSTEMD_USER_UNIT=zerobill.service -f`.
- **macOS:** run `zerobill run --apply` from a launchd agent.

Run it on a different machine than your runner when you can, so a power cut there does
not stop zerobill as well.

To watch zerobill itself, start it with `run --apply --health :8080`. It then serves
`/healthz`, which answers `200` while its loop started or finished a check in the last
15 minutes and `503` otherwise. Point an uptime monitor or a Kubernetes probe at it. It reports the
loop, not GitHub: read failures stay in the log, since restarting would not fix them.
