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

Think of three ways to get to school: a parent's car that is free but only when they
are home (your machine), a bus with 2,000 free rides a month (GitHub), and a carpool
with 3,000 (Blacksmith). Without zerobill you pick one ride for good, so you either wait
at the door for a car that is not coming, or spend bus rides on days the car was free.
zerobill checks every few minutes and changes the sign on the door to the free ride.

zerobill does two things. Every 5 minutes it sets the repository variable `CI_RUNNER`
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
BOT_RUNNER  blacksmith-2vcpu-ubuntu-2404  least used free tier: blacksmith 42%, github 53%

Repositories: 7 read CI_RUNNER or BOT_RUNNER.

Problems:
  fixed runner, change to ${{ vars.CI_RUNNER }}: your-org/api .github/workflows/release.yml:12: ubuntu-latest
```

## Install

```sh
go install github.com/morhaf-ko/zerobill/cmd/zerobill@latest
```

Or download a binary for Linux or macOS from the
[latest release](https://github.com/morhaf-ko/zerobill/releases/latest).

## Quick start

1. Sign in with the [GitHub CLI](https://cli.github.com/): `gh auth login`. zerobill uses
   that login, so there is no token to create.
2. Create `~/.config/zerobill/config.yaml`:

   ```yaml
   owner: your-org      # your GitHub user or organization
   ```

3. In your workflows, replace fixed labels such as `runs-on: ubuntu-latest` with:

   ```yaml
   runs-on: ${{ vars.CI_RUNNER }}
   ```

4. Check what zerobill sees: your free minutes, what it would choose, and any workflow
   still using a fixed label.

   ```sh
   zerobill status
   ```

5. Keep it running on a machine that stays on. Without `--apply` it only logs:

   ```sh
   zerobill run --apply
   ```

   To keep it running across reboots on Linux, use the systemd user service in
   [`examples/zerobill.service`](examples/zerobill.service).

## Options

| | Add to the config | Then |
| --- | --- | --- |
| **Your own runner** | `runner: home-ci` | Install GitHub's runner with that label; see the [self-hosted runner guide](docs/homelab.md). zerobill uses it first and moves jobs away once it stops. |
| **Blacksmith** | `blacksmith: true` | Install the [Blacksmith](https://www.blacksmith.sh/) GitHub app. Its 3,000 free minutes are shared with GitHub's so both last the month. |
| **Notifications** | `notify: https://ntfy.sh/your-topic` | Get a message whenever zerobill changes runner. Discord and Slack webhook URLs work too. |

Keep dependency bots on the hosted tiers, so your machine stays free for you:

```yaml
runs-on: ${{ github.actor == 'renovate[bot]' && vars.BOT_RUNNER || vars.CI_RUNNER }}
```

## Commands

| Command | |
| --- | --- |
| `zerobill status` | Free minutes, month-end projection, current choice and problems |
| `zerobill run [--apply]` | Keep `CI_RUNNER` and `BOT_RUNNER` on the best free runner |
| `zerobill pause runner 7d` | Take a runner out, for example while you travel; `zerobill resume runner` brings it back |

## FAQ

**What does it change?** Only the `CI_RUNNER` and `BOT_RUNNER` variables of private
repositories whose workflows read them. With `--apply` it also cancels and reruns jobs
stuck on a runner that is down. Nothing else.

**What access does it need?** Your GitHub CLI login, plus `gh auth refresh -s admin:org`
for an organization (or `-s user` for a personal account) to read runners and billing.
`zerobill status` tells you when that is missing. A `GITHUB_TOKEN` works too; see
[security](SECURITY.md).

**Can it cost me money?** It keeps 10% of every free tier in reserve and moves jobs off a
tier before it runs out. When every free tier is used, jobs wait for your runner. Without
a runner they stay on GitHub-hosted, which GitHub Free blocks at the allowance and paid
plans bill.

**Why not a scheduled workflow?** A workflow every 5 minutes on a private repository
would itself use at least 8,640 minutes a month.

**Public repositories?** They already get free hosted minutes, and GitHub
[advises against](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/about-self-hosted-runners#self-hosted-runner-security)
self-hosted runners on them, so zerobill only routes private repositories.

## More

[Reference](docs/reference.md) · [Self-hosted runner guide](docs/homelab.md) ·
[Contributing](CONTRIBUTING.md) · [Security](SECURITY.md) · [MIT license](LICENSE)
