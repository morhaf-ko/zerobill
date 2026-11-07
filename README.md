# zerobill

[![CI](https://github.com/morhaf-ko/zerobill/actions/workflows/ci.yml/badge.svg)](https://github.com/morhaf-ko/zerobill/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/morhaf-ko/zerobill)](https://github.com/morhaf-ko/zerobill/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**Keep GitHub Actions on free runners.** zerobill runs your private-repository CI on
your own machine when it is up, on free hosted minutes when it is not, and never on a
paid minute.

## Why

GitHub Free includes about 2,000 hosted Linux minutes a month for private repositories.
A home server or spare PC adds unlimited minutes, but only while it is on, and GitHub
never falls back between the two:

- A job sent to an offline self-hosted runner waits up to 24 hours, then is cancelled.
- A job sent to hosted runners after the free minutes are used is blocked or billed.
- Dependency bots open pull requests at night, when your machine is off.

zerobill sets one repository variable, `CI_RUNNER`, to the best free runner every few
minutes. Your workflows read it with `runs-on: ${{ vars.CI_RUNNER }}`. Jobs already stuck
on an offline runner are cancelled and rerun on the new one.

## Install

```sh
go install github.com/morhaf-ko/zerobill/cmd/zerobill@latest
```

Or download a binary for Linux or macOS from the
[latest release](https://github.com/morhaf-ko/zerobill/releases/latest), or use the image
`ghcr.io/morhaf-ko/zerobill`.

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

   For a service that survives reboots, use [`examples/zerobill.service`](examples/zerobill.service)
   (Linux) or [`examples/compose.yaml`](examples/compose.yaml) (Docker).

## Add free capacity

| | Add to the config | Then |
| --- | --- | --- |
| **Your own runner** | `runner: home-ci` | Install GitHub's runner with that label; see the [self-hosted runner guide](docs/homelab.md). zerobill uses it first and moves jobs away within about 15 minutes of it stopping. |
| **Blacksmith** | `blacksmith: true` | Install the [Blacksmith](https://www.blacksmith.sh/) GitHub app. Its 3,000 free minutes are shared with GitHub's so both last the month. |

Keep dependency bots on the hosted tiers, so your machine stays free for you:

```yaml
runs-on: ${{ github.actor == 'renovate[bot]' && vars.BOT_RUNNER || vars.CI_RUNNER }}
```

## Commands

| Command | |
| --- | --- |
| `zerobill status` | Free minutes, month-end projection, current choice, savings and problems |
| `zerobill run [--apply]` | Keep `CI_RUNNER` and `BOT_RUNNER` on the best free runner |
| `zerobill pause runner 7d` | Take a runner out, for example while you travel; `zerobill resume runner` brings it back |

## FAQ

**What does it change?** Only the `CI_RUNNER` and `BOT_RUNNER` variables of private
repositories whose workflows read them. With `--apply` it also cancels and reruns jobs
stuck on a runner that is down. Nothing else.

**What access does it need?** Your GitHub CLI login, plus `gh auth refresh -s admin:org`
for an organization (or `-s user` for a personal account) to read runners and billing.
`zerobill status` tells you when that is missing. A `GITHUB_TOKEN` works too; see the
[reference](docs/reference.md#access).

**Can it cost me money?** It keeps 10% of every free tier in reserve and moves jobs off a
tier before it runs out. When everything is used, jobs wait for your runner, or on
GitHub Free, stop at the free allowance.

**Public repositories?** They already get free hosted minutes, and GitHub
[advises against](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/about-self-hosted-runners#self-hosted-runner-security)
self-hosted runners on them, so zerobill only routes private repositories.

## More

[Reference](docs/reference.md) · [Self-hosted runner guide](docs/homelab.md) ·
[Design](docs/design.md) · [Contributing](CONTRIBUTING.md) · [Security](SECURITY.md) ·
[MIT license](LICENSE)
