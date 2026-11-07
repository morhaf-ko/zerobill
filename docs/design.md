# Design

## Problem

Private repositories on GitHub Free get 2,000 hosted Linux minutes a month. Blacksmith
adds 3,000 per organization. Home hardware is free per minute but costs electricity and
is often off, for example while an unattended agent pipeline runs overnight. GitHub has
no native fallback between self-hosted and hosted runners, and a job aimed at an offline
self-hosted label waits until GitHub cancels it after 24 hours.

## Approach

A small controller runs on an always-on host, such as a Raspberry Pi. On each interval it:

1. Collects provider state.
2. Runs the pure policy in `internal/policy` once per route. A route maps a set of
   variables to an ordered subset of providers, so dependency bots can be kept on free
   tiers while people's pushes use home hardware first. With `balance: pace` a route
   picks the usable metered provider with the smallest used share of its free minutes.
3. Writes each route's label to its repository variables that differ from it. A label
   stays for at least `min_hold` before moving to another usable provider; moving off
   one that is no longer usable is immediate. An interval with a
   failed collector writes nothing, so a transient API error cannot move jobs onto a
   metered provider.
4. Cancels runs queued on a self-hosted label that no route chose and whose provider is
   down, saturated or disabled, and reruns
   their failed and cancelled jobs on the next interval, once the cancellation has
   completed, so they pick up the new value. `runs-on` is evaluated when a job is
   queued, so queued jobs keep their label. Runs with a started job are left alone, and
   each run moves at most once, so a job with a literal `runs-on` cannot loop.

On-demand pools such as GARM or Actions Runner Controller register a runner only when
a job arrives, so an empty runner list is normal. For them, a job queued past
`max_queue_wait` is the outage signal, latched for `retry_after` or until a runner
comes online, since the queue drains once jobs move elsewhere.

The controller runs as a container or systemd service, not as a scheduled workflow: a
5-minute schedule on a private repository bills at least 8,640 minutes a month.

## Collectors

| Provider | Online and queue state | Used minutes |
| --- | --- | --- |
| Self-hosted | GitHub runner list for the label, plus queued `workflow_job` age | Not metered |
| GitHub-hosted | Always online | The organization or user billing usage API; confirm the endpoint for the account's billing platform |
| Blacksmith | Always online | Sum of job durations on `blacksmith-*` labels from the GitHub jobs API, times the size multiplier, rounded up per job |

Estimated minutes can drift from the vendor's bill. The reserve absorbs that drift.

## Milestones

1. Dry run: policy, config, `plan` from a snapshot. Then live collectors that log the
   decision on every interval without writing anything, giving a week of real usage.
2. Switching: write variables with change detection and a minimum hold time to avoid
   flapping, then rerun stuck jobs. Routes for separate workloads, pace balancing
   across free tiers, on-demand runner health and operator overrides.
3. Reports: a monthly summary of minutes per provider and estimated cost avoided.

## Non-goals

Paid overflow, multi-account free-tier farming, and replacing runner managers such as
GARM or Actions Runner Controller. zerobill only chooses between labels they provide.
