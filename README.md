# zerobill

[![CI](https://github.com/morhaf-ko/zerobill/actions/workflows/ci.yml/badge.svg)](https://github.com/morhaf-ko/zerobill/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/morhaf-ko/zerobill)](https://github.com/morhaf-ko/zerobill/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**Keep GitHub Actions on free runners.** zerobill runs your private-repository CI on
your own machine when it is up, and on free hosted minutes when it is not.

## Why

GitHub Free includes about 2,000 hosted Linux minutes a month for private repositories.
A home server or spare PC adds unlimited minutes, but only while it is on, and GitHub
never falls back between the two:

- A job sent to an offline self-hosted runner waits up to 24 hours, then is cancelled.
- A job sent to hosted runners after the free minutes are used is blocked or billed.
- Dependency bots open pull requests at night, when your machine is off.

## What you need

zerobill uses the GitHub API with one GitHub credential:

| Provider | Set up first | How zerobill reads it |
| --- | --- | --- |
| GitHub-hosted | Access to your private repositories and account billing | GitHub's billing API; organization billing requires an appropriate administrator role and an available billing report. |
| Your own runner (optional) | Install a self-hosted runner and give it a label | GitHub's runner and queued-job APIs. zerobill does not create runners. |
| Blacksmith (optional) | Install the [Blacksmith GitHub App](https://www.blacksmith.sh/) for your owner and repositories | Estimates usage from GitHub Actions job labels and durations. No Blacksmith API key or CLI is needed. |

The Blacksmith figure is an estimate, not an invoice. Only enable it if you already
have access to Blacksmith runners. See the [self-hosted runner guide](docs/homelab.md)
for your own machine and [security](SECURITY.md) for token permissions.

zerobill does two things. Every 5 minutes by default it sets the repository variable `CI_RUNNER`
to the best free runner, which your workflows read with
`runs-on: ${{ vars.CI_RUNNER }}`. And it reruns jobs that were already stuck on your
runner when it went down, on the new runner.

With your runner off, `zerobill status` shows where jobs go and how long each free tier
lasts:

```text
$ zerobill status
your-org, 7 Nov 21:30 UTC, 23% of the month gone

RUNNER      LABEL                         STATE                   USED      FREE      MONTH END AT THIS PACE
runner      home-ci                       offline                 -         -         -
blacksmith  blacksmith-2vcpu-ubuntu-2404  1570 free minutes left  1130 min  1570 min  4916 min, runs out 17 Nov
github      ubuntu-latest                 850 free minutes left   950 min   850 min   4133 min, runs out 14 Nov

VARIABLE    USE                           WHY
CI_RUNNER   blacksmith-2vcpu-ubuntu-2404  least used free tier: blacksmith 42%, github 53% (skipped runner: offline)

Repositories: 7 read CI_RUNNER.

Problems:
  fixed runner, change to ${{ vars.CI_RUNNER }}: your-org/api .github/workflows/release.yml:12: ubuntu-latest
```

## Install

For a published release, install Go and the [GitHub CLI](https://cli.github.com/), then run:

```sh
go install github.com/morhaf-ko/zerobill/cmd/zerobill@latest
zerobill version
```

`@latest`, [release binaries](https://github.com/morhaf-ko/zerobill/releases/latest)
and the container image follow published releases. The config rename and setup changes
in this branch are unreleased until PR #20 is merged and a new release is published.
To test them now, follow [local testing](docs/testing.md#local-testing); installing
`@latest` currently installs an older CLI. Run `zerobill version` to check your build.

Docker is optional. You can try zerobill on your existing computer. Automatic routing
needs `run --apply` to keep running; installing the CLI does not start it. Move it to
an existing always-on machine when ready; you do not need to rent a server.

## Quick start

1. Create your config:

   ```sh
   zerobill init
   ```

   Enter your GitHub user or organization, your runner label if you have one, and
   whether you use Blacksmith. Init writes your choices and comments out optional
   defaults. Existing configs stay untouched. It tells you how to sign in with the [GitHub CLI](https://cli.github.com/) if needed:

   ```sh
   gh auth login --hostname github.com
   ```

2. Check access and setup:

   ```sh
   zerobill status
   ```

   Status shows the GitHub account and credential source. When access or setup is
   incomplete, it explains the problem, prints the next commands and exits with
   status 1. It withholds usage estimates and recommendations until reads succeed.
   The first Blacksmith estimate can take a few minutes; later checks use a cache.

   Status prints its current phase and request count while it works. Press Ctrl+C
   to cancel; a workflow request timeout stops the scan instead of repeating the
   timeout across repositories.

3. Connect the private repositories you want to route. Create the variable before
   changing the workflow, so `runs-on` never reads an empty value:

   Use the repository-specific command printed by status. It initializes `CI_RUNNER`
   to `zerobill-wait`, so new jobs queue without selecting paid capacity. Never assign
   that label to a runner. After enabling routing, cancel and rerun any jobs already
   queued on that label. The command preserves the same
   credential that zerobill checked. Then edit that repository's workflow:

   ```yaml
   runs-on: ${{ vars.CI_RUNNER }}
   ```

   Run `zerobill status` again. It identifies workflows still using a fixed label
   and supplies a command using a discovered repository as an example.

4. Preview decisions on a machine that stays on:

   ```sh
   zerobill run
   ```

   This logs decisions every 5 minutes by default. After reviewing them, enable routing:

   ```sh
   zerobill run --apply
   ```

   `--apply` changes repository variables and can cancel and rerun stalled jobs.
   Keep this terminal open while testing. To survive logout and reboot, follow
   [running as a service](docs/service.md): native Linux or Docker, whichever you
   already use. Configure and authenticate on that machine; your laptop's settings
   are not automatically copied there.

## Which GitHub login is used?

The order is `GITHUB_TOKEN`, then `GH_TOKEN`, then your stored GitHub CLI login.
An environment token takes precedence even if `gh auth status` shows another account.
To try the stored CLI login for one command without changing your shell settings:

```sh
env -u GITHUB_TOKEN -u GH_TOKEN zerobill status
```

Status suggests `gh auth refresh` when GitHub denies required reads. Extra scopes
cannot grant an organization role or make an unavailable billing API available.
For containers, supply a token with the [required permissions](SECURITY.md).

## Config and defaults

Only `owner` is required. `zerobill init` creates a config with commented options;
uncomment a value to override its default. See [the example](examples/config.yaml).

```yaml
owner: your-org

# Optional: the GitHub Actions label assigned to your own runners.
# self_hosted_label: home-ci

# Enable after installing the Blacksmith GitHub App.
# blacksmith: false

# reserve_percent: 10
# alert_remaining_percent: 20
# check_interval: 5m
# queue_timeout: 10m
# notify: https://ntfy.sh/your-topic
```

Find your self-hosted label under **GitHub → Settings → Actions → Runners**. Use a
label shared only by machines you want zerobill to select. A laptop with a different
label stays out. Without `self_hosted_label`, zerobill still shares work between
GitHub and Blacksmith when enabled. Upgrading an existing config: rename `runner`
to `self_hosted_label`; for containers, rename `ZEROBILL_RUNNER` to
`ZEROBILL_SELF_HOSTED_LABEL`. Old names produce a migration error.

## How routing works

All jobs, including bots, can use `runs-on: ${{ vars.CI_RUNNER }}`:

1. Prefer usable self-hosted runners, returning on the next successful check.
2. Otherwise choose the hosted tier with the lowest percentage of its usable
   allowance consumed. With a 10% reserve, 2,000 GitHub minutes provide 1,800 usable
   minutes and 3,000 Blacksmith minutes provide 2,700. This is proportional sharing,
   rather than exhausting one provider first.
3. At the reserve, stop selecting that tier. When neither tier has capacity, jobs
   wait for your self-hosted runner, or queue on `zerobill-wait` without one. GitHub
   eventually cancels jobs that cannot find a runner; this is not indefinite storage.

`check_interval` controls availability and usage checks, default 5 minutes.
`queue_timeout` controls how long an on-demand self-hosted job may wait before its
pool is considered unavailable, default 10 minutes. Both accept durations from `1m`
to `1h`; shorter checks use more GitHub API requests. API processing adds time.
The 15-minute hold only reduces switching between usable hosted tiers; it never
holds an exhausted tier or delays returning to usable self-hosted capacity.

For an optional hosted-first bot policy, see [the reference](docs/reference.md#routing).

## Alerts and spending protection

Set `notify` to an ntfy topic, Discord webhook or Slack webhook. During
`run --apply`, zerobill sends a low-capacity warning once per provider per UTC month,
including after a restart. By default it warns at 20% of the full allowance remaining,
ahead of the 10% reserve, so you have time to turn on your own runners. Routing
changes also send messages. `status` and preview logs show low-capacity warnings
without sending those warnings.

Change `alert_remaining_percent` to 10 or 5 if desired, or 0 to disable it. Choose a
threshold above `reserve_percent` for advance notice; otherwise routing can stop
using a provider before usage reaches the warning threshold.

zerobill reduces paid overflow risk; it cannot guarantee zero charges. Usage can lag,
jobs already running keep running, fixed-label workflows bypass routing, and an
API failure or stopped service leaves the last variable value in place. Configure
provider spending limits as the financial backstop. On GitHub, use a budget with
[Stop usage when budget limit is reached](https://docs.github.com/en/billing/how-tos/set-up-budgets),
not an alert-only budget. Configure Blacksmith billing controls separately; zerobill
does not verify either provider's spending limits or manage storage charges.

Blacksmith and your own runners still depend on GitHub Actions. Switching compute
providers cannot bypass an Actions control-plane outage. zerobill currently detects
self-hosted queue stalls, not hosted-provider outages. Use `zerobill pause github`
or `zerobill pause blacksmith` on the machine running the controller to exclude a
hosted tier when GitHub's API is working.

## Commands

| Command | |
| --- | --- |
| `zerobill init` | Create a config with guided prompts; never overwrites an existing file |
| `zerobill status` | Free minutes, month-end projection, current choice and problems |
| `zerobill run [--apply]` | Keep connected workflows on available free capacity |
| `zerobill pause runner 7d` | Take a runner out, for example while you travel; `zerobill resume runner` brings it back. With Docker: `docker compose exec zerobill zerobill pause runner 7d` |

## FAQ

**What does it change?** Only the `CI_RUNNER` and `BOT_RUNNER` variables of private
repositories whose workflows read them. With `--apply` it also cancels and reruns jobs
stuck on a runner that is down. Nothing else.

**What access does it need?** GitHub access to private repositories, runners and billing.
`zerobill status` identifies the credential and gives commands for missing access.
Organization roles and fine-grained token permissions are listed in [security](SECURITY.md).

**Can it cost me money?** Yes. The reserve and waiting behavior help conserve free
capacity; provider spending limits are still required. See [spending protection](#alerts-and-spending-protection).

**Why not a scheduled workflow?** A workflow every 5 minutes on a private repository
would itself use at least 8,640 minutes a month.

**Public repositories?** They already get free hosted minutes, and GitHub
[advises against](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/about-self-hosted-runners#self-hosted-runner-security)
self-hosted runners on them, so zerobill only routes private repositories.

## More

[Service setup](docs/service.md) · [Reference](docs/reference.md) ·
[Self-hosted runner guide](docs/homelab.md) · [Testing](docs/testing.md) ·
[Contributing](CONTRIBUTING.md) · [Security](SECURITY.md) · [MIT license](LICENSE)
