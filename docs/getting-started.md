# Connect your first workflow

Start with one private repository. You need GitHub access to that repository and
the owner's billing report. Your own runner and Blacksmith are optional.

This walkthrough checks setup with the local CLI, then keeps one controller running
on the machine you choose. If you already have a deployed controller, inspect its
logs instead of starting another applied controller for the same owner.

## 1. Install and sign in

Install zerobill using [mise, a release archive or Go](../README.md#installation),
and install the [GitHub CLI](https://cli.github.com/). Then run:

```sh
zerobill version
gh auth login --hostname github.com
zerobill init
zerobill status
```

Enter the GitHub user or organization that owns the repository. Leave the
self-hosted label empty if you do not have a runner. Enable Blacksmith only after
installing its GitHub App for the owner and repository. zerobill estimates its usage
through GitHub; you do not need a separate Blacksmith key.

Check the account and credential source printed by status. The order is
`GITHUB_TOKEN`, `GH_TOKEN`, then your stored CLI login. To inspect the stored login
without environment tokens for one command:

```sh
env -u GITHUB_TOKEN -u GH_TOKEN zerobill status
```

If you keep that credential choice, use the same prefix on subsequent zerobill
commands. Fix reported access errors before continuing. Status prints commands to
check authentication and repository visibility. Organization access may also need
an administrator role; see [permissions](../SECURITY.md).

If no workflow is connected yet, status exits with 1 and explains that setup is
incomplete. That is expected at this step. After access succeeds, follow its
workflow instructions below.

## 2. Connect one private repository

Copy the repository-specific variable command printed by status. It creates
`CI_RUNNER` with value `zerobill-wait` and preserves the credential source status
checked. Never assign that reserved label to a runner.

Add this manual test as `.github/workflows/zerobill-check.yml` on the private
repository's default branch. It runs only when you dispatch it:

```yaml
name: Check zerobill routing

on:
  workflow_dispatch:

permissions: {}

jobs:
  check:
    runs-on: ${{ vars.CI_RUNNER }}
    timeout-minutes: 1
    steps:
      - name: Confirm the selected runner can execute
        run: printf 'Runner platform %s/%s\n' "$RUNNER_OS" "$RUNNER_ARCH"
```

This job checks out no code and imports no actions. The same diagnostic is available
as [examples/acceptance.yml](../examples/acceptance.yml) with a different filename.
After this test works, replace fixed `runs-on` labels in the CI jobs you want routed
with `${{ vars.CI_RUNNER }}`. Review that change through your normal PR process.
Repositories whose workflows do not read the routing variables, and fixed-label
jobs, remain outside routing.

Run `zerobill status` again. Once setup and reads succeed, it reports the connected
repository and a recommended label. Use only `CI_RUNNER` for the default policy;
`BOT_RUNNER` is an optional hosted-first policy for a separate use case.

## 3. Preview, then enable one controller

On the computer that will keep routing active:

```sh
zerobill run
```

Review the completed JSON log. `apply` is false. Check its `errors`, selected
`label` and `reason`. Leave `notify` unset for a preview without webhook messages.
The first Blacksmith scan can take several minutes; the controller emits its first
routing log after collection. Subsequent checks reuse completed-job history from
its state directory.

Stop preview with Ctrl+C. When the decisions are correct:

```sh
zerobill run --apply
```

Before applying, review the scope: the controller discovers every visible private
repository under the configured owner whose workflows read `CI_RUNNER` or
`BOT_RUNNER`. The test workflow does not limit it to one repository.

A completed check should have `apply` true and no `errors`. The `written` arrays
name repositories whose variables changed; an empty array means they already
matched. Inspect **Settings → Secrets and variables → Actions → Variables** on the
test repository to confirm its actual `CI_RUNNER` value.

For automatic startup and restart, choose the [Docker or Linux service guide](service.md).
Configure credentials on that machine. Docker uses its private `.env` settings;
it does not read the YAML config created by `init` on your laptop. Preserve the
state volume and run one applied controller per owner.

## 4. Run and inspect the test job

After `CI_RUNNER` selects available capacity, open the repository's **Actions** tab,
choose **Check zerobill routing**, and select **Run workflow**.

Open the resulting job. Verify its successful conclusion and runner name. With
self-hosting, the name must belong to your own pool and the requested label must
match `self_hosted_label`. A green job alone does not identify which provider ran it.

Jobs already queued on `zerobill-wait` keep that label. Cancel those test runs and
dispatch a new one after routing selects capacity; changing the variable does not
move the old run. Avoid dispatching tests before the first routing check completes.

## 5. Check self-hosted recovery

If you use a self-hosted runner, start its existing service or pool. Zerobill reads
its registration from GitHub; it needs no IP address or direct connection to your
machine. Matching online capacity is preferred on the next successful check. The
default interval is 5 minutes; `check_interval: 1m` makes checks more frequent.
GitHub reporting and collection time also affect detection.

An empty matching pool is assumed to start runners on demand. A log reason such
as `no runner registered; expecting runners per job` is an assumption, not an online
check. Require `observed.runner.online: true` and `reason: online`, then run the test
workflow again to prove execution. An idle runner in an existing GARM/ARC pool
makes returning capacity observable; see the [runner guide](homelab.md).

To test fallback, use a dedicated test owner and controller with no other workloads.
On that controller, pause the runner, wait for a successful hosted choice and run
the test once. For a native CLI:

```sh
zerobill pause runner 10m
# After the completed controller check and hosted test, restore preference:
zerobill resume runner
```

For Compose, run those commands through `docker compose exec zerobill zerobill`.
Pauses affect every connected repository on that controller and are read on its
next check. Do not pause a shared production controller just to test this tutorial.
A hosted test can be billed; check free capacity and provider spending limits first.
[Isolated testing](testing.md#private-repository-acceptance) covers the full sequence.

## When something looks wrong

| Symptom | Next step |
| --- | --- |
| Status says setup is incomplete | Follow its `Next` commands; check owner, account, repository visibility and permissions |
| First scan has no routing log yet | Allow the initial monthly scan to finish; preserve state instead of repeatedly restarting |
| A label is selected but a job is queued | Check GitHub's runner list and job labels; an empty pool may be treated as on-demand |
| A variable changed but an old job still waits | Inspect that run's original labels; dispatch a fresh manual test |
| Local status differs from the VPS | Inspect the deployed controller's config and logs; local status makes separate API reads |
| A provider cannot be read | Repair the reported access or API failure; failed reads leave existing variables unchanged |

## Stop or roll back

Stop a foreground controller with Ctrl+C. For a service, follow its
[stop and rollback commands](service.md). Stopping leaves GitHub variables at their
last values. Before removing zerobill, explicitly restore each workflow's desired
fixed label or repository-variable value so new jobs have available capacity.

Keep provider spending controls enabled. Reserves, estimates and alerts reduce
paid overflow risk; they cannot guarantee a zero bill or recover a GitHub Actions
control-plane outage.
