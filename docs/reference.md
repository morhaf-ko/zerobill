# Reference

## Config

`~/.config/zerobill/config.yaml`, or `$XDG_CONFIG_HOME/zerobill/config.yaml`:

| Key | Meaning |
| --- | --- |
| `owner` | Required. The GitHub user or organization whose private repositories zerobill routes. |
| `runner` | The label of your self-hosted runners. Leave it out without one. |
| `blacksmith` | `true` to use Blacksmith's free minutes too. |

State lives in `~/.local/state/zerobill` (`$XDG_STATE_HOME/zerobill`): a cache of this
month's jobs, the last decision per variable, and pauses.

## Access

zerobill uses `GITHUB_TOKEN` when it is set, and otherwise `gh auth token`.

| Need | GitHub CLI login | Fine-grained token |
| --- | --- | --- |
| Repositories, workflows, runs and jobs | default `gh auth login` | Repository: Contents, Actions and Metadata read |
| Runners and billing of an organization | `gh auth refresh -s admin:org` | Organization: Self-hosted runners and Administration read |
| Runners and billing of a personal account | `gh auth refresh -s user` | Repository: Administration read; Account: Plan read |
| `run --apply` | default `gh auth login` | Repository: Actions and Variables write |

Give a fine-grained token access to all repositories: minutes are summed across them.

## Conventions

| Rule | Value |
| --- | --- |
| Repositories | Private repositories whose workflows read `vars.CI_RUNNER` or `vars.BOT_RUNNER`, rescanned hourly |
| `CI_RUNNER` | Your runner, then Blacksmith, then GitHub-hosted (`ubuntu-latest`) |
| `BOT_RUNNER` | Blacksmith, then GitHub-hosted |
| Free minutes | GitHub: 2,000 on Free, 3,000 on Pro and Team, read from your plan. Blacksmith: 3,000. |
| Reserve | 10% of each free tier is never used |
| Sharing | Among usable free tiers, the one with the smallest share used, so all last the month |
| Nothing free | Jobs wait for your runner, or use GitHub-hosted without one |
| Interval | Every 5 minutes (`run --interval`) |

## How a runner counts as down

- A registered runner with your label is offline, or
- a job has waited for the label more than 10 minutes. Runners created per job, such as
  GARM or Actions Runner Controller pools, register only when a job arrives, so this is
  how zerobill notices they are gone. That stall counts for an hour, or until a runner
  comes online.
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

- Linux: [`examples/zerobill.service`](../examples/zerobill.service), a systemd user
  service that uses your GitHub CLI login.
- Docker: [`examples/compose.yaml`](../examples/compose.yaml). The image reads
  `/etc/zerobill/config.yaml` and keeps state in `/var/lib/zerobill`; pass
  `GITHUB_TOKEN=$(gh auth token)`.
- macOS: a launchd agent running `zerobill run --apply`.

Run it on a different machine than your runner when you can, so a power cut there does
not stop zerobill as well.

## Verify a download

Each release publishes `checksums.txt` and a GitHub build attestation:

```sh
shasum -a 256 --ignore-missing -c checksums.txt
gh attestation verify zerobill_<version>_<os>_<arch>.tar.gz --repo morhaf-ko/zerobill \
  --signer-workflow morhaf-ko/zerobill/.github/workflows/release.yml
gh attestation verify oci://ghcr.io/morhaf-ko/zerobill:<version> --repo morhaf-ko/zerobill \
  --signer-workflow morhaf-ko/zerobill/.github/workflows/release.yml
```
