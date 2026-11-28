# Implementation verification

## Request fixture

`go test ./cmd/zerobill -run TestPollingRequestBudget -count=1 -v` drives 60
one-minute controller checks per case with 15 private repositories and one
variable per repository. Each workflow discovery sees two files per repo.
The busy case has six attempts of one run with 12 active-run job observations.
The rollover case switches billing months after 30 measured ticks. All calls
go to a deterministic in-process REST fixture. The counts are measured HTTP
attempts and counts of statuses other than 304. The fixture does not measure
actual GitHub charges or latency and returns matching ETags on most unchanged
responses. It excludes startup and workflow recovery mutations.

| Quiet hour endpoint class | Baseline attempts | Implemented attempts | Implemented non-304 |
| --- | ---: | ---: | ---: |
| Organization runners | 60 | 60 | 0 |
| Queued runs | 900 | 900 | 15 |
| In-progress runs | 900 | 900 | 15 |
| Repository inventory | 61 | 60 | 0 |
| Runs created this month | 900 | 900 | 15 |
| GitHub billing usage | 60 | 60 | 60 |
| Variables | 900 | 180 | 15 |
| Workflow directories | 15 | 15 | 0 |
| Workflow files | 30 | 0 | 0 |
| Total | 3,826 | 3,075 | 120 |
| Mean per tick | 63.77 | 51.25 | 2.00 |

| Fixture | Baseline attempts and non-304 | Step 2 attempts and non-304 | Final attempts | Final non-304 |
| --- | ---: | ---: | ---: | ---: |
| Quiet | 3,826 | 3,075 | 3,075 | 120 |
| Busy | 3,844 | 3,093 | 3,093 | 148 |
| Rollover | 3,826 | 3,075 | 3,075 | 135 |

The busy case adds six all-attempt jobs requests (six 200) and 12 latest jobs
requests (six 200 and six 304). Changed run views add five 200 responses for
runs-created and eleven for in-progress lists compared with the quiet case.
Month rollover adds 15 new 200 responses for the new runs-created query.

The quiet baseline and step 2 match the [design](design.md). Step 3 keeps live
capacity and inventory reads, so attempts remain higher than the design's
2,259/hour projection. Holding this implementation's 60 unconditional billing
reads fixed, a 75% ETag hit rate among the other 3,015 quiet requests would
result in about 814 charges per hour. No validator support would leave all
3,075 quiet attempts potentially chargeable. The real hit rate and shared
bucket acceptance require comparable production windows.

## Capacity, recovery and identity

The existing billing report cannot quantify unreported or in-flight usage.
The Blacksmith history can omit older re-runs and does not establish a bound
from effective concurrency and vCPU mix. Keep those collectors live, including
a current owner inventory for full Blacksmith coverage. Discovery reuses that
inventory when the scans coincide. A release with an evidence-backed bound
can revisit the five-minute projection.

The tests cover a billing request crossing midnight UTC, Retry-After across
checks, partial variable writes, periodic drift, pagination after 304, and
later jobs queuing under an unchanged active run. A full terminal inventory
page is read unconditionally so a new next link can be found. Large history
responses cannot evict active-run validators. Interrupted discovery resumes
at its next unfinished repository. An interrupted Blacksmith collection
retains completed attempts but withholds a capacity result until a complete
check. The controller fixture runs the whole sequence on one-minute ticks:
it finds a later job, switches the variable on the first tick beyond its
queue timeout, cancels, submits and verifies the replacement attempt, and
switches back on the tick when the idle runner returns.

The existing installation cannot be selected for runtime delivery without
proof of complete repository coverage, the exact organization billing report
and an approved renewable installation-token source. No credentials, grants
or App configuration are required for this source change. Revisit the
[installation gates](design.md#installation-identity) only if the production
measurements justify it. Events still need webhook ownership and would not
remove the required runner checks, so there is no event pilot in this change.

## Production acceptance

Use the v0.6 telemetry release for the baseline. Compare quiet and busy windows
with the reduced-volume release at comparable completed checks. Inspect
per-endpoint status counts, latency, health, routing/recovery logs and the
operator's independently measured shared quota. Apply and rollback through
the platform's existing version declaration with its tag and digest.

### Personal REST core quota

The operator added this acceptance on 2026-11-28 when retiring the dotfiles
`give-agents-github-identity` mission. Run it after the reduced-volume ZeroBill
release is healthy in production and the operator is doing normal multi-mission
work with the personal GitHub CLI identity. Use the existing personal
credential and its shared `core` bucket.

Run the sampler in the background. Take a sample at the start and once per
minute through the one-hour mark. Each sample calls `gh api rate_limit` once
and records its UTC timestamp and `resources.core` limit, remaining and
reset time. Read the sampler output once, after it finishes. Record the
window, number of samples, minimum remaining, any quota resets and the
pass or fail result here and in the polling PR evidence. A window passes
when every sample is strictly above 2,500 remaining. A missing sample
leaves acceptance unverified.

Result pending the reduced-volume production rollout.
