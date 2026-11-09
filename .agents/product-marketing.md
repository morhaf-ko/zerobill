# Product marketing context

Document version: v1
Last updated: 2026-11-09

## Product and audience

zerobill is an MIT-licensed Go CLI and continuous controller for private GitHub
Actions workflows. It conserves hosted minutes by preferring configured self-hosted
capacity and sharing usable GitHub and optional Blacksmith allowances.

The intended audience is developers and small teams already using GitHub Actions.
They may have an intermittently available home runner or use hosted runners only.
The primary action is to connect one private workflow and verify its actual runner.
Audience demand and adoption have not been measured.

## Positioning and voice

Lead with conserving hosted CI minutes. Explain the single CI_RUNNER variable and
where the controller runs before listing options. Use plain technical language and
concrete commands. Treat self-hosting and Blacksmith as optional.

Avoid promises of zero charges, guaranteed savings, immediate detection, automatic
hosted-outage failover or independence from GitHub. Do not invent users, customer
quotes, savings figures, installation times or competitor comparisons.

## Supported claims

The README and docs/reference.md describe current routing. Source tests cover
policy, reads, writes and recovery. Release checksums and attestations identify
published artifacts. Private operator evidence must not enter public examples or
launch copy. An operator test does not establish general reliability or savings.

Provider spending limits remain necessary. Blacksmith usage is estimated from job
history. A configured empty self-hosted pool is initially treated as on-demand;
selecting its label does not establish that a machine is online.

## Promotion

Prepare organic open-source announcements linked to the getting started guide.
Publishing posts, contacting people and buying ads require separate instructions.
No paid service or billing integration is part of this scope.

## Learnings

No audience or campaign results have been recorded.

## Changelog

- v1 (2026-11-09): Established source-backed positioning and claim limits for documentation and launch copy.
