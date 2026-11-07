# zerobill

[![CI](https://github.com/morhaf-ko/zerobill/actions/workflows/ci.yml/badge.svg)](https://github.com/morhaf-ko/zerobill/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/morhaf-ko/zerobill)](https://github.com/morhaf-ko/zerobill/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Keep private-repository GitHub Actions free: run CI on your own machine when it is up,
on free hosted minutes when it is not, and never on a paid minute.

## The problem

Private repositories on GitHub Free get about 2,000 hosted Linux minutes a month. A
home server or spare PC gives you unlimited free minutes, but only while it is on.
GitHub has no fallback between the two:

- A job sent to an offline self-hosted runner waits up to 24 hours, then is cancelled.
- A job sent to hosted runners after the free minutes run out is blocked or billed.
- Dependency bots such as Renovate open PRs at night, when your machine is off.

## How zerobill solves it

Workflows read their runner from a repository variable:

```yaml
runs-on: ${{ vars.CI_RUNNER }}
```

zerobill runs on an always-on host. Every few minutes it checks your runners and your
free minutes, then sets `CI_RUNNER` to the best free option:

1. Your own runner, while it picks up jobs.
2. Otherwise a hosted free tier with minutes left: GitHub, Blacksmith or both. With
   several, it spreads jobs so each lasts the month.
3. Otherwise your own runner again, so jobs wait instead of costing money.

Jobs already stuck on an offline runner are cancelled and rerun on the new runner. No
routing job runs inside your workflows, so routing itself costs no minutes.

## Quick start

You need Docker on a machine that stays on (a Raspberry Pi, NAS or small VPS) and a
GitHub token (see [Token](#token)).

1. In each workflow, select the runner with `runs-on: ${{ vars.CI_RUNNER }}`.
2. Copy [`examples/zerobill.yaml`](examples/zerobill.yaml) and
   [`examples/compose.yaml`](examples/compose.yaml) into one folder. Edit `owner`,
   `repositories` and `providers`: keep your runner's label and delete providers you do
   not use. List only private repositories; GitHub
   [advises against](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/about-self-hosted-runners#self-hosted-runner-security)
   self-hosted runners on public ones.
3. Put `GITHUB_TOKEN=...` in `zerobill.env` and run `chmod 600 zerobill.env`.
4. Start it. It begins in `dry-run` and changes nothing:

   ```sh
   docker compose up -d
   docker compose logs -f zerobill
   ```

5. When the decisions look right, set `mode: switch` in `zerobill.yaml` and run
   `docker compose restart zerobill`.

Without Docker, download a binary from the
[latest release](https://github.com/morhaf-ko/zerobill/releases/latest) and run
`zerobill watch --config zerobill.yaml`, for example under
[`examples/zerobill.service`](examples/zerobill.service).

## Check it works

1. **Offline:** `zerobill plan --config examples/zerobill.yaml --observed examples/observed.yaml`
   prints the decision for a hand-written snapshot.
2. **Dry run:** each log line shows what zerobill observed and chose, and writes
   nothing. Compare `observed` with your billing page and runner list; `errors` should
   be absent.

   ```json
   {"mode":"dry-run","routes":[{"name":"ci","provider":"home","label":"home-ci","reason":"online","written":null}],
    "observed":{"home":{"online":true},"github":{"used_minutes":703}}}
   ```

3. **Fallback:** `docker compose exec zerobill zerobill disable home --for 1h` makes the
   next line choose a hosted provider; `zerobill enable home` undoes it.
4. **Switching:** in a throwaway private repository, run `switch` mode with a
   self-hosted label that no runner serves, and dispatch a workflow. zerobill switches
   `CI_RUNNER` once the job has waited past `max_queue_wait`, cancels the run and
   reruns it on the hosted runner. See
   [testing switch mode](docs/reference.md#testing-switch-mode).

## Features

- **Routes:** give dependency bots their own variable so they never use your machine:
  `runs-on: ${{ github.actor == 'renovate[bot]' && vars.BOT_RUNNER || vars.CI_RUNNER }}`.
- **Pace balancing** across several free tiers (`balance: pace`).
- **On-demand runners** (GARM, Actions Runner Controller) count as down only when a job
  waits too long (`on_demand: true`).
- **Travel mode:** `zerobill disable home --for 7d`.
- **Minimum hold** (`min_hold`, default 15 minutes) stops flapping between providers.

## Providers

| Provider | Label example | Free minutes | How zerobill counts usage |
| --- | --- | --- | --- |
| Your own runner | any label | Unlimited | Runner online and queue age |
| GitHub-hosted | `ubuntu-24.04` | About 2,000 a month on Free | GitHub billing usage report |
| [Blacksmith](https://www.blacksmith.sh/) (optional) | `blacksmith-2vcpu-ubuntu-2404` | 3,000 a month | Job durations from the GitHub API |

GitHub-hosted alone is enough. Blacksmith keeps running and bills past its free
minutes, so keep a `reserve_minutes` margin if you use it.

## Token

zerobill reads `GITHUB_TOKEN`. For an organization, a fine-grained token needs:

| Permission | Why |
| --- | --- |
| Organization: Self-hosted runners, read | Runner status |
| Organization: Administration, read | GitHub-hosted minutes |
| Repository: Actions, read and write | Queued jobs; cancel and rerun in switch mode |
| Repository: Variables, read and write | Write `CI_RUNNER` in switch mode |
| Repository: Metadata, read | List repositories |

In `dry-run`, read access is enough. A classic token needs `repo` and `admin:org` for an
organization, or `repo` and `user` for a personal account.

## Fair use

zerobill uses free tiers you already have. It does not create accounts or
organizations to collect more free minutes; that breaks provider terms.

## More

- [Reference](docs/reference.md): every config key, the decision rules and switching.
- [Design](docs/design.md) and roadmap.
- [Contributing](CONTRIBUTING.md) and [security](SECURITY.md).

Built by Morhaf Kourbaj: [GitHub](https://github.com/morhaf-ko) ·
[LinkedIn](https://www.linkedin.com/in/mkourbaj/). [MIT](LICENSE) licensed.
