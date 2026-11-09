# Run zerobill continuously

You need one computer that stays on, not necessarily a new server. An existing
Linux machine, NAS or VPS works. Docker is optional. Run only one controller for
an owner: two controllers can overwrite each other's decisions and state.

Try the [quick start](../README.md#quick-start) with one private repository first.
Use the same release version for your CLI, config format and deployed image.
For unreleased changes, follow [local testing](testing.md#local-testing) rather than
mixing a development config with an older published binary or image.

## Choose where to run it

The controller checks GitHub and updates `CI_RUNNER`. Your self-hosted machine
executes jobs. They can share a machine, but switching it off also stops routing.
An existing always-on machine separate from your runner lets zerobill keep working
when that runner is off. No inbound port or connection to the runner is required.

Choose the native Linux service if you already use Linux services. Choose Docker
if you already run containers. A laptop is fine for testing while it stays awake;
installing zerobill does not start a background service.

## Native Linux service

Run these steps as the user who will own the service, on that machine. Install the
GitHub CLI, then prepare the binary directory for either installation method:

```sh
mkdir -p "$HOME/.local/bin"
export PATH="$HOME/.local/bin:$PATH"
```

Choose one installation method:

- With Go installed: `GOBIN="$HOME/.local/bin" go install github.com/morhaf-ko/zerobill/cmd/zerobill@latest`.
- Or download and verify a [release archive](https://github.com/morhaf-ko/zerobill/releases),
extract it, and install its binary with `install -m 755 ./zerobill "$HOME/.local/bin/zerobill"`.
Archives include this guide and the service examples. To test an unreleased checkout,
replace the Go install command with `GOBIN="$HOME/.local/bin" go install ./cmd/zerobill`.

Authenticate and configure on this machine:

```sh
gh auth login --hostname github.com
zerobill version
zerobill init
env -u GITHUB_TOKEN -u GH_TOKEN zerobill status
```

Follow status's access and workflow instructions. `GITHUB_TOKEN` and `GH_TOKEN`
override the CLI login; the service does not inherit exports from an interactive
shell. The example uses the stored CLI login. Its PATH searches `~/.local/bin`,
`/usr/local/bin`, `/usr/bin` and `/bin`. Check `command -v gh`; if GitHub CLI is
elsewhere, add its absolute directory to the unit's `Environment=PATH` setting.

From the extracted archive or checkout, install the example:

```sh
mkdir -p "$HOME/.config/systemd/user"
cp examples/zerobill.service "$HOME/.config/systemd/user/zerobill.service"
systemctl --user daemon-reload
systemctl --user enable --now zerobill.service
journalctl --user -u zerobill.service -n 20 --no-pager
```

It starts in preview mode. Logs show `"apply":false`; no repository variables are
written. If checks fail, repair the reported access or config before enabling writes.
Once the choices are correct, run `systemctl --user edit zerobill.service` and add:

```ini
[Service]
ExecStart=
ExecStart=%h/.local/bin/zerobill run --apply
```

Then restart and allow this user's service to survive logout:

```sh
systemctl --user restart zerobill.service
loginctl enable-linger "$USER"
systemctl --user status zerobill.service
journalctl --user -u zerobill.service -n 20 --no-pager
```

Enabling linger can require administrator approval. Verify logs show `"apply":true`
and no errors. Empty `written` arrays mean the variable already matched, not that
routing failed. Use `journalctl --user -u zerobill.service -f` to follow checks.

After changing config, run `zerobill status`, then restart the service. To update,
repeat the binary installation for the chosen release and restart. To roll back,
install the previous binary and restore a config it supports before restarting;
the old `runner` key became `self_hosted_label` in the new config format.

To stop routing:

```sh
systemctl --user disable --now zerobill.service
```

Stopping leaves the last repository-variable values in place. It does not stop
jobs already running or disable provider billing.

## Docker Compose

Docker uses the published image and environment settings. It does not read the
YAML config created by a CLI on your laptop. This avoids mounting the CLI's private
config file into a container running under a different user.

From an extracted release archive or checkout, create a new service directory:

```sh
mkdir -m 700 "$HOME/zerobill-service"
cp examples/compose.yaml "$HOME/zerobill-service/compose.yaml"
install -m 600 examples/env.example "$HOME/zerobill-service/.env"
cd "$HOME/zerobill-service"
```

Edit `.env` locally. Set `GITHUB_TOKEN` to a credential with the
[required permissions](../SECURITY.md), and set `ZEROBILL_OWNER`. Uncomment
`ZEROBILL_SELF_HOSTED_LABEL` if you have self-hosted runners and
`ZEROBILL_BLACKSMITH=true` only after installing its GitHub App. Optional defaults
are commented in the example. Keep `.env` private and out of Git.

The token belongs on the service machine; the container cannot use your laptop's
GitHub CLI login. Shell environment values override `.env`, so unset conflicting
exports if the reported credential or owner differs from what you intended.

Validate without printing credentials, then check setup:

```sh
docker compose config --quiet
docker compose run --rm zerobill status
```

Follow the reported access and workflow instructions. Preview continuously:

```sh
docker compose up -d
docker compose logs --tail 20 zerobill
```

After reviewing the decisions, edit `compose.yaml` from `command: ["run"]` to
`command: ["run", "--apply"]`, then apply that change:

```sh
docker compose up -d
docker compose logs --tail 20 zerobill
```

Verify `"apply":true` and no errors. The named volume keeps cache, pauses, routing
state and sent-warning records across restarts. Config changes require
`docker compose up -d` to recreate the container. To update to a published release,
set the same chosen version in your CLI install and image tag, then run:

```sh
docker compose pull
docker compose up -d
```

For rollback, restore the previous image tag and compatible `.env` settings, then
run those two commands. Old images use `ZEROBILL_RUNNER`; the new config uses
`ZEROBILL_SELF_HOSTED_LABEL`. To stop without removing state:

```sh
docker compose down
```

Do not add `--volumes` unless you intend to erase state. As with the native service,
stopping leaves the last GitHub variables in place.

## Control the instance you deployed

Log in to the service machine with SSH. For a native service, run `zerobill status`,
`zerobill pause runner 7d` or `zerobill resume runner` as its owning user. With Docker,
run commands in its Compose directory:

```sh
docker compose exec zerobill zerobill status
docker compose exec zerobill zerobill pause runner 7d
docker compose exec zerobill zerobill resume runner
```

Pauses take effect on the next check. Status makes its own fresh reads; use service
logs to confirm the running controller's last check and applied choices. Running a
second `run --apply` process to force a check can race the first controller. Immediate
refresh and a local CLI that connects to a remote server are not implemented.

The default check interval is 5 minutes, configurable down to 1 minute. The 15-minute
hosted-routing hold and optional `/healthz` endpoint are separate from runner polling;
see [availability rules](reference.md#how-a-runner-counts-as-down). Zerobill's `/healthz`
checks its loop, not your server's power state or GitHub access.

Configure provider spending limits separately. Logs, reserves and alerts help you
notice problems; they do not provide a billing guarantee.
