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

The guided workflow in this README follows `main`. Install it with:

```sh
go install github.com/morhaf-ko/zerobill/cmd/zerobill@main
zerobill version
```

`@latest` installs the newest tagged release, which can be older than `main`.
[Release binaries](https://github.com/morhaf-ko/zerobill/releases/latest) and the
image `ghcr.io/morhaf-ko/zerobill` also follow published releases.

## Quick start

1. Create your config:

   ```sh
   zerobill init
   ```

   Enter your GitHub user or organization, your runner label if you have one, and
   whether you use Blacksmith. Init leaves existing configs untouched and tells
   you how to sign in with the [GitHub CLI](https://cli.github.com/) if needed:

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

3. Connect the private repositories you want to route. Create the variable before
   changing the workflow, so `runs-on` never reads an empty value:

   ```sh
   gh variable set CI_RUNNER --body ubuntu-latest --repo your-org/your-repo
   ```

   ```yaml
   runs-on: ${{ vars.CI_RUNNER }}
   ```

   Run `zerobill status` again. It identifies workflows still using a fixed label
   and supplies a command using a discovered repository as an example.

4. Preview decisions on a machine that stays on:

   ```sh
   zerobill run
   ```

   This logs decisions every 5 minutes. After reviewing them, enable routing:

   ```sh
   zerobill run --apply
   ```

   `--apply` changes repository variables and can cancel and rerun stalled jobs.
   For a persistent service, use [Docker](examples/compose.yaml) or the
   [systemd user service](examples/zerobill.service).

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

## Options

| | Add to the config | Then |
| --- | --- | --- |
| **Your own runner** | `runner: home-ci` | Install GitHub's runner with that label; see the [self-hosted runner guide](docs/homelab.md). zerobill uses it first and moves jobs away once it stops. |
| **Blacksmith** | `blacksmith: true` | Install the [Blacksmith](https://www.blacksmith.sh/) GitHub app. Its 3,000 free minutes are shared with GitHub's so both last the month. |
| **Notifications** | `notify: https://ntfy.sh/your-topic` | Get a message whenever zerobill changes runner. Discord and Slack webhook URLs work too. |
| **Reserve** | `reserve_percent: 10` | Keep this percentage of each free tier in reserve. Default: 10; accepts integers from 0 to 99. Zero uses the full allowance. |

Keep dependency bots on the hosted tiers, so your machine stays free for you:

```yaml
runs-on: ${{ github.actor == 'renovate[bot]' && vars.BOT_RUNNER || vars.CI_RUNNER }}
```

## Commands

| Command | |
| --- | --- |
| `zerobill init` | Create a config with guided prompts; never overwrites an existing file |
| `zerobill status` | Free minutes, month-end projection, current choice and problems |
| `zerobill run [--apply]` | Keep `CI_RUNNER` and `BOT_RUNNER` on the best free runner |
| `zerobill pause runner 7d` | Take a runner out, for example while you travel; `zerobill resume runner` brings it back. With Docker: `docker compose exec zerobill zerobill pause runner 7d` |

## FAQ

**What does it change?** Only the `CI_RUNNER` and `BOT_RUNNER` variables of private
repositories whose workflows read them. With `--apply` it also cancels and reruns jobs
stuck on a runner that is down. Nothing else.

**What access does it need?** GitHub access to private repositories, runners and billing.
`zerobill status` identifies the credential and gives commands for missing access.
Organization roles and fine-grained token permissions are listed in [security](SECURITY.md).

**Can it cost me money?** By default it keeps 10% of every free tier in reserve;
change that with `reserve_percent`. This buffer helps cover running jobs and delayed
usage reporting; it cannot guarantee zero charges. When every free tier is used, jobs wait for your runner. Without
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
