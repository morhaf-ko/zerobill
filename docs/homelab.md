# Self-hosted runner guide

A spare machine, such as an old laptop, a mini PC or a gaming PC you are not using, can
run your CI for free. It needs only outbound HTTPS; GitHub never connects to it.

## 1. Install GitHub's runner

Use GitHub's own runner, so no third-party code holds your registration token.

1. Open **Settings → Actions → Runners → New self-hosted runner** on your organization,
   or on a repository for a personal account.
2. Run the commands GitHub shows, adding a label for zerobill:

   ```sh
   ./config.sh --url https://github.com/YOUR_ORG --token TOKEN_FROM_THAT_PAGE --labels home-ci --unattended
   ```

3. Run it as a service so it starts with the machine:

   ```sh
   sudo ./svc.sh install && sudo ./svc.sh start
   ```

4. Install Docker if your jobs use `services:` or `docker build`.

Keep it safe:

- Private repositories only. On a public repository, a fork's pull request could run code
  on your machine. Organization runner groups block public repositories by default.
- Run it under its own user, with nothing it should not reach, such as SSH keys.

## 2. Tell zerobill

```yaml
owner: your-org
runner: home-ci
```

`zerobill status` shows the runner as `online`. Jobs go to it first; when it is off they
move to your free hosted minutes, and come back when it is on again.

## Travel

Before switching the machine off for a while, pause it so jobs go straight to the hosted
tiers instead of waiting 10 minutes first:

```sh
zerobill pause runner 14d
```

`zerobill resume runner` brings it back early.

## Runners created per job

GARM or Actions Runner Controller pools work too. Use the label your jobs target. No
runner is registered between jobs, so zerobill treats a job waiting 10 minutes as the
pool being down.
