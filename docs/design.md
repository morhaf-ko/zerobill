# Design

## Problem

Private repositories on GitHub Free get 2,000 hosted Linux minutes a month. Blacksmith
adds 3,000 per organization. Home hardware is free per minute but is often off. GitHub
has no fallback between self-hosted and hosted runners, and a job aimed at an offline
self-hosted label waits until GitHub cancels it after 24 hours.

## Approach

Workflows read their runner from a repository variable, so one controller can move
every job by changing that variable. zerobill is that controller, and it keeps one job:
choose the free runner. It runs on any always-on machine and, every interval:

1. Finds the private repositories whose workflows read `CI_RUNNER` or `BOT_RUNNER`.
2. Reads your runner's state, GitHub's billing usage and an estimate of Blacksmith
   minutes from job durations.
3. Runs the pure policy in `internal/policy` for both variables.
4. With `--apply`, writes the variables that differ, then cancels and reruns jobs stuck
   on a runner that is down. An interval with a failed read writes nothing.

Everything a user would otherwise configure is a convention: two variables, a 10%
reserve, a 10-minute queue limit, a 15-minute hold, sharing free tiers by the share
used. The config holds only who you are and which free capacity you have. Access comes
from the GitHub CLI login, so setup needs no token.

The controller runs as a service, not as a scheduled workflow: a 5-minute schedule on a
private repository would bill at least 8,640 minutes a month.

## Non-goals

Paid overflow, multi-account free-tier farming, and replacing runner managers such as
GARM or Actions Runner Controller. zerobill only chooses between labels they provide.
