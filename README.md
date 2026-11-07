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

You need a GitHub token and Docker on a machine that stays on, such as a Raspberry Pi,
NAS or small VPS. No self-hosted runner yet? See the [homelab guide](docs/homelab.md).

1. **Token.** Create a fine-grained token with read access
   ([personal account](https://github.com/settings/personal-access-tokens/new?name=zerobill&expires_in=90&actions=read&actions_variables=read&contents=read&metadata=read&administration=read&plan=read),
   [organization](https://github.com/settings/personal-access-tokens/new?name=zerobill&expires_in=90&actions=read&actions_variables=read&contents=read&metadata=read&organization_self_hosted_runners=read&organization_administration=read):
   pick the organization as resource owner) and give it access to all repositories.
   `export GITHUB_TOKEN=...`
2. **Config.** `init` reads your private repositories, workflows, runners and
   variables, and writes `zerobill.yaml` and `compose.yaml`:

   ```sh
   alias zerobill='docker run --rm -e GITHUB_TOKEN -u "$(id -u)" -v "$PWD:/work" -w /work ghcr.io/morhaf-ko/zerobill:0.3.0'
   zerobill init --owner YOUR_USER_OR_ORG
   ```

3. **Check.** `zerobill doctor` tests each permission and lists every `runs-on` that
   zerobill cannot route yet, with the line to change, such as
   `runs-on: ${{ vars.CI_RUNNER }}`. It changes nothing.
4. **Run.** Put `GITHUB_TOKEN=...` in `zerobill.env`, run `chmod 600 zerobill.env`, then
   `docker compose up -d`. It starts in `dry-run`: it logs a decision every 5 minutes
   (`docker compose logs -f zerobill`) and writes nothing.
5. **Switch.** When the decisions look right, give the token write access to Actions
   and Variables, set `mode: switch` in `zerobill.yaml` and run
   `docker compose restart zerobill`.

List only private repositories; GitHub
[advises against](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/about-self-hosted-runners#self-hosted-runner-security)
self-hosted runners on public ones. Without Docker, download a binary from the
[latest release](https://github.com/morhaf-ko/zerobill/releases/latest) and use
[`examples/zerobill.service`](examples/zerobill.service).

## See your budget

`zerobill status` shows each provider's minutes, what is left before the reserve, the
month-end projection at the current pace, each route's choice and what your own runners
saved:

```text
PROVIDER    KIND         STATE                            USED  LIMIT  LEFT  MONTH-END AT THIS PACE
home        self-hosted  online                           1004  -      -     -
blacksmith  metered      1611 free minutes above reserve  1089  2700   1611  4780, reserve on 17 Nov
github      metered      890 free minutes above reserve   910   1800   890   3994, reserve on 14 Nov

Saved this month: about $6.02, self-hosted minutes priced at GitHub's $0.006 a minute.
```

With Compose: `docker compose exec zerobill zerobill status`. Each log line also carries
`saved_usd_estimate`.

## Check it works

1. **Dry run:** each log line shows what zerobill observed and chose. Compare
   `observed` with your billing page and runner list; `errors` should be absent.
2. **Fallback:** `docker compose exec zerobill zerobill disable home --for 1h` makes the
   next decision choose a hosted provider; `zerobill enable home` undoes it.
3. **Switching:** in a throwaway private repository, run `switch` mode with a
   self-hosted label that no runner serves and dispatch a workflow. zerobill switches
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

zerobill reads `GITHUB_TOKEN`. `zerobill doctor` names any missing permission and prints
a link that pre-fills a token for your owner.

| Permission | Why |
| --- | --- |
| Repository: Actions, read (write for switch mode) | Queued jobs and minutes; cancel and rerun |
| Repository: Variables, read (write for switch mode) | Read and set `CI_RUNNER` |
| Repository: Contents, read | `init` and `doctor` read workflow files |
| Repository: Metadata, read | List repositories |
| Organization: Self-hosted runners, read | Runner status (organizations) |
| Organization: Administration, read | GitHub-hosted minutes (organizations) |
| Repository: Administration, read | Runner status (personal accounts) |
| Account: Plan, read | GitHub-hosted minutes (personal accounts) |

Give it access to all repositories: own-runner and Blacksmith minutes are summed across
them. A classic token with `repo` plus `admin:org` (organization) or `user` (personal
account) also works.

## Fair use

zerobill uses free tiers you already have. It does not create accounts or
organizations to collect more free minutes; that breaks provider terms.

## More

- [Reference](docs/reference.md): every config key, the decision rules and switching.
- [Design](docs/design.md) and roadmap.
- [Contributing](CONTRIBUTING.md) and [security](SECURITY.md).

Built by Morhaf Kourbaj: [GitHub](https://github.com/morhaf-ko) ·
[LinkedIn](https://www.linkedin.com/in/mkourbaj/). [MIT](LICENSE) licensed.
