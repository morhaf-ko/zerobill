# Test before releasing

## Local testing

The configuration and setup changes in PR #20 are not in v0.3.5. Installing
`@latest` or pulling the published image does not test this candidate.

With Go and Git installed, start from a checkout:

```sh
git clone --branch simplify-routing-config https://github.com/morhaf-ko/zerobill.git
cd zerobill
go run ./cmd/zerobill version
```

If you already have this branch checked out, use it directly. Create a separate
config so testing does not alter your existing setup:

```sh
export GH_CONFIG_DIR="${GH_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/gh}"
export XDG_CONFIG_HOME="$(mktemp -d)"
export XDG_STATE_HOME="$(mktemp -d)"
go run ./cmd/zerobill init
```

`GH_CONFIG_DIR` preserves your existing GitHub CLI login before changing the XDG
location; only zerobill's config and state are isolated. Run these commands in a separate test shell and close it when done.

Inspect `$XDG_CONFIG_HOME/zerobill/config.yaml`. Only your owner and selected options
should be active; optional defaults are commented. To use an existing config, copy
it into this isolated directory and rename `runner` to `self_hosted_label`.

Read-only status and preview use your normal GitHub credential:

```sh
go run ./cmd/zerobill status
go run ./cmd/zerobill run
```

Press Ctrl+C to stop preview. Preview does not write GitHub variables, but a configured
`notify` URL receives routing-change messages ending in `[log only]`. Leave `notify`
unset for an entirely read-only preview. Low-capacity warnings are logged, not sent,
until `--apply`.

For a local installed command, run `go install ./cmd/zerobill`. This installs the
checkout, bypassing cached `@main` lookups. Only one applied controller may manage
an owner. Do not run this candidate with `--apply` alongside a deployed controller.
For unreleased Docker testing, build a local candidate image; do not use the old
published image with the new settings.

## Private-repository acceptance

Use a dedicated private test repository and a credential restricted to it, with the
organization runner and billing reads zerobill needs. A full-access credential could
let `--apply` discover and update other connected repositories under the owner.
Do not use an existing production controller or shut down a shared runner for this test.

Copy [the acceptance workflow](../examples/acceptance.yml) into
`.github/workflows/acceptance.yml` on the test repository's default branch. It is
manual-only, imports no actions, checks out no code, and runs one bounded diagnostic
job on `CI_RUNNER`. Set up the variable using status's command before dispatching it.
Keep private repository names, credentials and job logs out of public evidence.

Check this sequence with isolated controller config and state:

1. Run `init`; verify defaults, file permissions and overwrite refusal.
2. Run `status`; follow any access instructions and verify the expected owner,
   credential, private repository and self-hosted label.
3. Run `run --apply` and inspect its log plus the actual `CI_RUNNER` variable.
   Dispatch the acceptance workflow; inspect GitHub's job record to confirm the
   selected labels and successful execution on your self-hosted runner.
4. Pause `runner` on this isolated controller. Wait for its next successful check;
   confirm a hosted label was applied and dispatch the workflow again. Verify its
   actual runner labels and success. This tests policy fallback without powering
   down a shared machine. Resume the runner and verify self-hosted preference returns.
5. Stop and restart the controller; verify it uses the same config/state and does
   not send another already-recorded low-capacity warning.

Do not consume an allowance to test exhaustion or send alerts to real operator
webhooks. The local HTTP-fixture journey covers reserve/exhaustion, runner offline,
low-capacity delivery/retry, restart deduplication and failed reads without spending
provider minutes. A live hosted job can still be billed; verify provider limits and
available free capacity before dispatching it.

## Release gates

Run the repository checks:

```sh
go vet ./...
go test -race ./...
```

CI also checks GoReleaser configuration, workflow syntax and automation security.
The native CLI setup smoke, local container startup and private-repository job
acceptance are separate gates: a green test suite does not prove a deployed service
or a real job ran. Record each as passed, failed or unrun.

Before tagging: merge the reviewed exact head, verify main CI, remove the unreleased
setup notices from the README and service guide, and verify the release's supported
config format. After tagging: verify release checksums and attestations, and smoke
the published binary and container image. Follow [security](../SECURITY.md) for
verification commands. A release candidate is not a published release or deployment.

Immediate refresh, remote-client profiles and automatic hosted-outage failover are
outside this release's periodic-routing scope.
