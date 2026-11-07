# Homelab guide

Two ways to keep private-repository CI free with zerobill. Path A adds your own
machine, which makes most CI minutes free. Path B uses hosted free tiers only and
still stops surprise bills.

## Path A: a spare machine as your runner

Any always-plugged-in Linux or macOS machine works: an old laptop, a mini PC, a gaming
PC while you are not using it. It only needs outbound HTTPS; GitHub never connects to it.

### 1. Install GitHub's runner

Use GitHub's own runner, so no third-party code holds your registration token:

1. Open **Settings → Actions → Runners → New self-hosted runner**, on the organization
   for all its repositories or on one repository for a personal account.
2. Run the download and `./config.sh` commands GitHub shows, adding a label for
   zerobill to route to:

   ```sh
   ./config.sh --url https://github.com/YOUR_ORG --token TOKEN_FROM_THE_PAGE --labels home-ci --unattended
   ```

3. Run it as a service, so it starts with the machine:

   ```sh
   sudo ./svc.sh install && sudo ./svc.sh start
   ```

4. Install Docker on the machine if your jobs use `services:` or `docker build`, and give
   the runner's user access to it.

Keep it safe:

- Use it for private repositories only. On a public repository a fork's pull request
  could run code on your machine. Organization runner groups block public repositories
  by default; keep it that way.
- Run it under its own user with nothing else on the machine it should not reach, such
  as SSH keys or a password manager.
- Jobs on the same runner share a disk; workflows should not leave secrets in the
  workspace.

Runners created per job by GARM or Actions Runner Controller also work; `init` marks
them `on_demand: true`.

### 2. Point your workflows at a variable

```yaml
runs-on: ${{ vars.CI_RUNNER }}
```

`zerobill doctor` lists every `runs-on` still fixed to a label. Until zerobill sets the
variable in switch mode, set it yourself once per repository:
`gh variable set CI_RUNNER --body home-ci --repo OWNER/REPO`.

### 3. Run zerobill somewhere that stays on

Follow the [quick start](../README.md#quick-start). zerobill is one small process that
makes a few dozen API calls every 5 minutes.

Run it on a different machine than the runner when you can, such as a Raspberry Pi or a
small VPS. If both share a machine, a power cut or network outage there stops zerobill
too, and jobs wait for the runner instead of moving to a hosted tier.

## Path B: hosted free tiers only

Without a machine of your own, list only metered providers. With both GitHub and
Blacksmith and `balance: pace`, zerobill spreads jobs so neither runs out early, and
stops before either one's reserve:

```yaml
fallback: github
providers:
  - {name: blacksmith, label: blacksmith-2vcpu-ubuntu-2404, kind: metered, monthly_minutes: 3000, reserve_minutes: 300}
  - {name: github, label: ubuntu-latest, kind: metered, monthly_minutes: 2000, reserve_minutes: 200}
routes:
  - {name: ci, variables: [CI_RUNNER], providers: [blacksmith, github], balance: pace}
```

Blacksmith keeps running and bills past its free minutes, so keep its
`reserve_minutes`. GitHub Free stops hosted jobs at the included usage unless a budget
allows more, so the GitHub fallback cannot bill you there.

## Keep dependency bots off your machine

Renovate and Dependabot open many small PRs, often at night when your machine is off.
Give them their own variable:

```yaml
runs-on: ${{ github.actor == 'renovate[bot]' && vars.BOT_RUNNER || vars.CI_RUNNER }}
```

and a route that only uses hosted free tiers:

```yaml
  - {name: bot, variables: [BOT_RUNNER], providers: [blacksmith, github], balance: pace}
```

An unset `BOT_RUNNER` falls back to `CI_RUNNER`, so the workflow change is safe before
zerobill runs.

## Travel

Before switching the machine off, take it out of rotation so jobs go straight to a
hosted tier instead of waiting `max_queue_wait` first:

```sh
docker compose exec zerobill zerobill disable home --for 14d
```

`zerobill enable home` brings it back early.

## See what you saved

`zerobill status` prints each provider's minutes, the month-end projection and an
estimate of what your runner saved at GitHub's hosted price.
