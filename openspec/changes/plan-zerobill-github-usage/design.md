## Production evidence

On 2026-11-28, a read-only deployment inspection confirmed v0.6.0, organization
routing, Blacksmith enabled, apply mode and a one-minute interval. Retained logs
had 1,205 controller entries across the previous 25 hours. Thirteen reported a
rate limit between 22:06 UTC on November 27 and 10:09 UTC on November 28.
A subsequent 20-minute sample had 17 applied ticks with 15 managed repositories,
all three providers observed, zero errors and zero changed variables.
These are measured ticks and outcomes. The logs omit successful request counts,
pagination and response headers. The incident investigation also reported an
805-request drop across ten minutes in a shared user window and verified the
service's authenticated user. The drop measures the whole bucket. A subsequent
read-only quota query on the operator's CLI reported 421 remaining out of 5,000.
That point sample cannot attribute calls to the service.
[Platform runbook](https://github.com/morhaf-labs/vps-infra-v2/blob/main/docs/zerobill.md)
· [GitHub REST rate limits](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api)

## Requests by endpoint

Counts are inferred from v0.6.0. Each page is a request. Let **T** be managed
repositories (15), **B** be all non-archived repositories in the organization
visible to the credential (at least T, including public), **P** be private
repositories scanned for workflow discovery (at least T), and **V** be managed
repository/variable pairs (15–30 with the two supported variables).
A failed tick can stop a collector early or skip apply. There is no implicit
whole-tick retry. Failed top-level discovery can run again on the next tick.

| REST endpoint | When and count | Preferred reduction and trade-off |
| --- | --- | --- |
| `GET /users/{owner}` | 1 at startup to determine owner type | Keep it once per process. |
| `GET /orgs/{org}/actions/runners` | At least 1 per tick | Keep the one-minute poll and try ETag per page. Preserve label, status and busy state. Workflow events cannot report idle runners returning. The personal-owner path uses `/repos/{repo}/actions/runners` at least T times and follows the same rule. |
| `GET /repos/{repo}/actions/runs?status=queued` | At least T per tick | Keep at one minute with ETags. A cached empty result still needs revalidation. |
| `GET /repos/{repo}/actions/runs?status=in_progress` | At least T per tick | Same protection. Later jobs can queue within a running workflow. GitHub's filter accepts one status. An unfiltered recent list could omit a long-running attempt or cost more pages. |
| `GET /repos/{repo}/actions/runs/{id}/jobs?filter=latest` | One or more pages per unique active run encountered across the two status scans in each repo | Keep at one minute with ETags even if the run list is unchanged. Current code already deduplicates run IDs across statuses. Live queue state and attempt identity govern freshness. |
| `GET /orgs/{org}/repos?type=all` | At least 1 per tick for Blacksmith, plus its own paginated scan at startup and roughly hourly for discovery | Share one owner-scoped snapshot between consumers. Refresh at most every five minutes in the proposed steady state and validate pages with ETags. For a personal owner the endpoint is `/user/repos?affiliation=owner`. |
| `GET /repos/{repo}/actions/runs?created=…` | At least B per tick for Blacksmith. Above the 1,000-result cap, one initial request plus per-day paginated queries | Move to five-minute capacity collection with freshness guards. ETags or overlap-based incremental reconciliation can skip unchanged history only after covering late jobs, old reruns and month rollover. Keep organization-wide coverage. |
| `GET /repos/{repo}/actions/runs/{id}/jobs?filter=all` | One or more pages for each run attempt absent from the completed-attempt cache | Keep the completed-attempt cache and apply the slower capacity schedule. The latest-only response omits older attempts and cannot substitute for all attempts. Coalesce reads only if the endpoint, parameters, authority and freshness are equivalent. |
| `GET /organizations/{org}/settings/billing/usage?year=…&month=…` | 1 per tick for GitHub-hosted usage. A personal owner uses `/users/{owner}/settings/billing/usage` | Refresh every five minutes when reserve guards permit it. Test validators and billing lag on representative responses. Use the existing billed SKU and unit. |
| `GET /repos/{repo}/contents/.github/workflows`, `GET /repos/{repo}/contents/{file}` | P directory requests plus one per YAML file at startup and roughly hourly | Keep hourly fallback discovery. Compare directory entries and fetch/parse only new or changed blob SHAs. Cache missing directories only for a bounded interval. A default-branch push or repository event can invalidate sooner. |
| `GET /repos/{repo}/actions/variables/{name}` | V reads on every successful apply tick even when values match | Recheck every five minutes when the desired route stays stable. Read immediately on a route change, first sight, restart, or uncertain write. Consider paginated `GET …/actions/variables` when both variables are needed and the list costs fewer calls. Use ETags where supported. |
| `POST /repos/{repo}/actions/variables`, `PATCH /repos/{repo}/actions/variables/{name}` | One write per missing or changed variable after its read | Keep idempotent comparison and GET error handling. Preserve the order of verified variable updates before run recovery. GitHub generally restricts conditional requests to reads. |
| `GET /repos/{repo}/actions/runs/{id}`, `GET /repos/{repo}/actions/runs/{id}/attempts/{n}/jobs` | A new move can take 2 run GETs plus job pages. A pending recovery takes 1 run GET per eligible tick and can add job pages and a final run recheck. An expired remembered run takes 1 GET per eligible tick until cleared | Keep fresh reads before a mutation and verify the replacement attempt. Keep active recovery at one minute. Only slow cleanup of expired deduplication records, retaining them until verified. |
| `POST /repos/{repo}/actions/runs/{id}/{cancel,force-cancel,rerun-failed-jobs}` | At most one action for that run per applicable stage and tick | Keep guarded mutations with durable intent records and follow-up verification. An uncertain POST requires reconciliation. |

[Collector](https://github.com/morhaf-ko/zerobill/blob/v0.6.0/internal/collect/collect.go)
· [Controller](https://github.com/morhaf-ko/zerobill/blob/v0.6.0/cmd/zerobill/run.go)
· [Discovery](https://github.com/morhaf-ko/zerobill/blob/v0.6.0/internal/discover/discover.go)
· [Variables writer](https://github.com/morhaf-ko/zerobill/blob/v0.6.0/internal/apply/apply.go)
· [Recovery writer](https://github.com/morhaf-ko/zerobill/blob/v0.6.0/internal/apply/move.go)
· [REST pagination](https://docs.github.com/en/rest/using-the-rest-api/using-pagination-in-the-rest-api)
· [Actions runs](https://docs.github.com/en/rest/actions/workflow-runs)
· [Actions jobs](https://docs.github.com/en/rest/actions/workflow-jobs)
· [Variables](https://docs.github.com/en/rest/actions/variables)
· [Repository contents](https://docs.github.com/en/rest/repos/contents)

The earlier 48-read floor counted observation: 1 runner page + 15 queued
queries + 15 in-progress queries + 1 owner inventory page + at least 15
Blacksmith month queries + 1 billing report. Apply adds at least 15 variable
GETs, for **63 reads per successful tick**, or **3,780/hour at 60 ticks**.
Pagination, job lookups, additional org repositories, the second variable,
discovery, recovery and mutations add to that figure. Tick overruns and failed
reads can lower requests issued per elapsed hour.

## Rollout order and request budget

The table models 60 successful checks, 15 managed and 15 visible repositories,
15 variables, one page per selector, no active jobs or recovery, one discovery
with 15 directories and 30 workflow files at baseline, and unchanged workflow
blobs after cache warm-up. A busy hour adds job pages, writes, capacity refreshes
and recovery. The model is a comparison, with no claim about the production
ETag hit rate or organization inventory size.

| Step | HTTP requests/hour | Primary REST charges/hour | Basis |
| --- | ---: | ---: | --- |
| v0.6.0 reference hour | 3,826 | 3,826 | 3,780 steady reads plus 46 discovery reads. Startup adds owner lookup and may trigger cold collection. |
| 1. Measure at the existing client | 3,826 | 3,826 | Count every issued request by endpoint class, method and status. Capture sanitized quota and validator statistics from existing responses. |
| 2. Share inventory, reuse workflow blobs, reconcile stable variables every five minutes | 3,075 | 3,075 | Save 720 variable GETs, 30 unchanged file GETs and 1 duplicate inventory GET. Variable drift is corrected within five minutes of a healthy check. Route changes still reconcile immediately. |
| 3. Refresh capacity and inventory every five minutes with freshness guards | 2,259 | 2,259 | Save 816 scheduled requests for 15 month scans, 1 inventory page and 1 billing report every five minutes versus every minute. Includes 1,860 one-minute runner and run-list GETs. |
| 4. Add ETag revalidation to scheduled reads | 2,259 | About 574 at a 75% hit rate | Assume the 12 billing reads are unconditional. Of the 2,247 remaining reads, 50% and 90% hit rates give about 1,136 and 237 charges respectively. Missing validators can take the cost back to 2,259. |
| 5. Optionally migrate to an existing App installation | About 2,259 plus 1–2 token mints | About 574 installation charges for the same workload, plus any charge applicable to minting | Zero recurring personal-bucket requests once all operations, including billing, use the installation. Token minting uses App authentication. |
| Optional event pilot after proving demand | About 2,259 for the same idle hour | About 574 at the same hit rate | Keep one-minute polling in the initial pilot. Events can reduce follow-up reads in active hours after parity and delivery testing. Budget no additional savings up front. |

Implement and measure each step in order. Aim for at least a 50% decrease in
charged requests per hour at comparable tick counts and workload, with a target
under 2,500 in a representative busy hour without delaying failover. Report
HTTP attempts independently. Include a quiet hour, a busy hour with reruns, and
a cache-cold or month-rollover exercise. Compare per-endpoint totals and latency
with the baseline. Capture `x-ratelimit-limit`, `remaining`, `used`, `reset` and
`resource` and count 200/304/403/429 by endpoint class. Attribute requests using
the client counters and status. Headers reveal the credential's shared quota
and may differ between regions. Exclude tokens, raw payloads and repo identifiers
from telemetry. Use GitHub's reset and retry guidance. Back off on limits and
keep routes unchanged on incomplete required observations.
[REST API best practices](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api)
· [REST rate limits](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api)

## Failover and cache behavior

Start runner/queue work every minute. Keep the configured queue timeout, hosted
hold and recovery guards. Recompute queued age from original job timestamps
against the current clock even on a 304. Keep both active status filters and
check jobs of active runs for new queued steps. Schedule slow capacity/discovery
work outside the critical path, bound its request budget and give failover
first use of API capacity. Compare end-to-end observation and update latency
against the current controller. Target the next successful one-minute check
plus API processing after GitHub exposes the relevant runner or queue change.

Use bounded, credential-scope-aware cache keys covering the full URL and API
representation. Save the body and next-link metadata with each ETag. Revalidate
every page in the current link chain and discard no-longer-linked pages. A 304
requires a matching complete cached response. Retry once unconditionally
within budget if that response is missing. Flush relevant entries on writes,
visibility/permission changes or credential scope changes. An authorized 304
saves primary REST quota. It still costs an HTTP request and network time.
Confirm support and semantics for each endpoint before assigning savings.
[Conditional requests](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api#use-conditional-requests)

For scheduled capacity, use a complete same-month snapshot and a conservative
usage bound for its age, including in-flight work and the provider's reporting
lag. Prove the bound against observed usage and effective concurrency. Near a
reserve, refresh sooner. If no safe bound is available, preserve that provider's
current fresh-read behavior until verified. Missing, expired, incomplete or
previous-month capacity cannot authorize a hosted route or a rerun. Exercise
cold start, month rollover, collection errors and a provider reaching reserve
between ticks. For variable reconciliation, track each repo/variable, retain
retry eligibility after partial writes, and force applicable variables through
fresh verification before a recovery action. Periodically check out-of-band
variable drift and give operators a documented way to trigger reconciliation.

## Webhooks and GraphQL

GitHub recommends webhooks where they can replace polling. Treat
`workflow_job` queued/in-progress/completed and `workflow_run` as hints to
invalidate or refresh affected run state. Default-branch `push` and repository
or installation changes can accelerate discovery. The REST Events feed has
documented delivery lag and offers no one-minute failover guarantee. Billing,
variable drift and idle runner availability still need authoritative reads.
Revisit a webhook pilot only if the polling changes leave a material gap and
there is approved ingress and event ownership. Use signature verification,
delivery-ID deduplication, prompt acknowledgement and asynchronous handling.
A missed, delayed or reordered event must fall back to REST within the
one-minute queue observation budget. Keep fresh pre-mutation reads.
[Webhook events](https://docs.github.com/en/webhooks/webhook-events-and-payloads)
· [Webhook best practices](https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks)
· [Failed deliveries](https://docs.github.com/en/webhooks/using-webhooks/handling-failed-webhook-deliveries)
· [REST Events](https://docs.github.com/en/rest/activity/events)

GraphQL can batch repository metadata and Git objects across repositories.
Consider it only if discovery remains costly after SHA reuse. Compare fetched
pages, GraphQL points and latency against conditional REST for the same
authorized inventory and workflow content. The Actions schema exposes
`WorkflowRun` and `CheckSuite`; it does not establish parity for status-filtered
repository scans, attempt-scoped jobs with runner assignment and execution
evidence, or runner capacity. Keep REST for the hot path, billing, variables
and recovery unless an equivalent query is demonstrated. GraphQL consumes a
separate primary point budget with its own query limits and secondary limits.
[GraphQL Actions schema](https://docs.github.com/en/graphql/reference/actions)
· [GraphQL Git schema](https://docs.github.com/en/graphql/reference/git)
· [GraphQL rate and query limits](https://docs.github.com/en/graphql/overview/rate-limits-and-query-limits-for-the-graphql-api)

## Installation identity

Reduce usage first. A GitHub App installation gives the workload a distinct
primary REST bucket, starting at 5,000/hour for standard installations and
scaling for eligible installations. Other uses of the same installation share
that bucket. An App user token and another personal access token for the same
user both spend the user's allowance. The existing platform release workflow
shows an App-token mint with narrowly scoped repository access. It establishes
a pattern for short-lived tokens. ZeroBill's required grants and runtime refresh
remain to be verified. Continue on the reduced-volume personal credential if
the existing App cannot safely cover the required calls.
[Existing platform workflow](https://github.com/morhaf-labs/vps-infra-v2/blob/main/.github/workflows/deploy-to-platform.yml)
· [GitHub REST limits by identity](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api)
· [Installation token minting](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app)

| Operation | Installation requirement |
| --- | --- |
| Owner type and organization inventory | Metadata read. Compare complete visible inventory with the existing scope. Where needed, inspect `GET /installation/repositories` to confirm selected-repository access. |
| Workflow discovery | Contents read for the relevant private repositories. |
| Organization runner status | Organization Self-hosted runners read. The repository-runner variant for a personal owner requires repository Administration read. |
| Workflow runs, jobs and recovery | Actions read for observation and Actions write for cancel, force-cancel and rerun. |
| Runner variables | Variables read/write on managed repositories. |
| Organization usage report | GitHub documents installation token support with organization Administration read on the enhanced billing platform. Validate the exact monthly report with the existing installation and compare usage with the current credential. For a personal owner the user billing endpoint requires a user-scoped Plan permission. |

[Installations](https://docs.github.com/en/rest/apps/installations)
· [Self-hosted runners](https://docs.github.com/en/rest/actions/self-hosted-runners)
· [Billing usage](https://docs.github.com/en/rest/billing/usage?apiVersion=2022-11-28)
· [Actions runs](https://docs.github.com/en/rest/actions/workflow-runs)
· [Actions variables](https://docs.github.com/en/rest/actions/variables)

Operator steps if the migration is approved in a later mission:

1. In the existing App and organization installation settings, confirm its
   identity, owners, current grants and repository selection. Compare with the
   endpoint matrix and org-wide Blacksmith coverage. Decide whether any
   additional grant is acceptable, including organization Administration read.
   Changes to App permissions can affect other consumers and may require the
   installation owner's acceptance. Pause the migration if the available
   installation scope would omit metered repositories or broaden access beyond
   the accepted boundary.
2. Approve a scoped runtime token source through the existing platform owner.
   Keep the App private key in its approved signing store and give ZeroBill
   only installation tokens scoped to the required repos and permissions.
   Confirm the platform has an owned way to mint and refresh them. If a binding
   or key is required, the operator provisions it via the reviewed platform
   secret contract. v0.6.0 reads a fixed token at launch. A one-hour token
   pasted into its current Doppler entry would stop working after expiry.
   The implementation must refresh before expiry, test renewals and fail
   closed on an expired token. Token failures must alert without leaking
   credentials or falling back silently to the personal identity.
3. Approve a preview and the eventual production cutover through the platform
   runbook. Compare read coverage and usage first, then prove variable and
   recovery permissions with a disposable owned test repository under separate
   authority. Verify over at least one token renewal before cutover. Preserve
   the one-writer deployment and recovery journal. Keep a rollback path via the
   reviewed previous image and credential binding, and have the operator retire
   the old credential only after the rollback window. Reuse the existing
   personal binding for rollback through the runbook if required.
   [Platform runbook and Doppler commands](https://github.com/morhaf-labs/vps-infra-v2/blob/main/docs/zerobill.md)
   · [GitHub App private keys](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/managing-private-keys-for-github-apps)
