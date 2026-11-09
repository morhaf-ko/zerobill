# Launch copy

Drafts for maintainer review. Publish after the chosen release's archive and image
checks pass. Publishing posts or changing repository metadata is a separate action.

## Repository description

Conserve GitHub Actions minutes with self-hosted runner preference and GitHub/Blacksmith free-tier routing.

Suggested topics: `github-actions`, `self-hosted-runner`, `ci`, `golang`, `blacksmith`.

## Short announcement

zerobill helps make your GitHub Actions minutes last longer.

It prefers your self-hosted runner when usable, then shares available GitHub and
optional Blacksmith free minutes. Connect a private workflow through
`runs-on: ${{ vars.CI_RUNNER }}` and keep one controller running on an existing
always-on machine.

Self-hosting and Blacksmith are optional. The CLI guides setup, shows usage and
supports configurable reserves and low-capacity alerts. Provider spending limits
remain necessary; routing cannot guarantee zero charges.

Try it with one private repository:
https://github.com/morhaf-ko/zerobill/blob/main/docs/getting-started.md

MIT licensed. Feedback on setup and routing is welcome.

## Longer announcement

GitHub Actions keeps the runner label a workflow asked for. If your home runner
goes offline, a job can remain queued while hosted capacity is available. Fixed
hosted labels can also keep consuming minutes when your own machine is ready.

The default zerobill setup uses one repository variable, CI_RUNNER, for the
private workflows you connect. It checks GitHub's runner status and usage, prefers usable self-hosted
capacity, and otherwise shares the remaining GitHub and optional Blacksmith free
allowances. It can cancel and rerun eligible jobs stalled on self-hosted capacity.

A hosted-only setup works too. You can configure the reserve, check interval,
queue timeout and low-capacity warning. Blacksmith usage is estimated from GitHub
job history; zerobill needs no separate Blacksmith API key.

The controller runs on an existing always-on computer through Docker or a native
Linux service. Your workflows continue to run through GitHub Actions. Start with
the guided CLI setup and one manual test workflow before connecting more jobs.

Usage reporting can lag, and running jobs continue. Set provider spending limits
as the financial backstop. Automatic hosted-outage failover is outside the tool's
current scope.

Source and installation: https://github.com/morhaf-ko/zerobill
Tutorial: https://github.com/morhaf-ko/zerobill/blob/main/docs/getting-started.md

## Demo outline

Use a synthetic owner and a dedicated private test repository. Keep credentials,
private names, webhook URLs and production logs out of screenshots.

1. Show `zerobill init` creating the commented config.
2. Show status identifying the account and explaining an unconnected workflow.
3. Create the variable using status's command and connect the manual diagnostic.
4. Show a completed applied check, then the actual GitHub job's runner name.
5. Demonstrate fallback only on an isolated test controller with approved capacity.
6. Show the spending-limit reminder and link to the tutorial.

Caption synthetic fixture output as synthetic. Do not claim measured savings,
customer adoption, real hosted failover or outage recovery from a self-hosted smoke.
