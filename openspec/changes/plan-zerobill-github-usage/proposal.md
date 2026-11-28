## Why

ZeroBill v0.6.0 can use at least 3,780 REST reads in an hour of successful
one-minute apply ticks for 15 managed repositories. It shares a user's
5,000-request hourly limit in the reference deployment. During the November
2026 exhaustion incident, GitHub errors prevented routing updates.

## What changes

- Keep runner and queue checks on the one-minute schedule. Reduce variable,
  discovery and capacity reads and revalidate hot reads with ETags.
- Measure both HTTP attempts and primary-quota usage. Evaluate an existing
  GitHub App installation after reducing request volume.
- Define acceptance for an implementation mission. This change proposes
  behavior, budgets and verification. Implementation and rollout require
  separate missions.

## Capabilities

### New capabilities

- `github-polling` covers request budgeting and fresh runner failover signals.

## Impact

Potential implementation owners are ZeroBill's GitHub client, discovery,
collector and apply paths. The service and reference docs will need to reflect
the implemented freshness and drift behavior. The existing deployment owner
handles any credential or rollout change through its
[ZeroBill runbook](https://github.com/morhaf-labs/vps-infra-v2/blob/main/docs/zerobill.md).
