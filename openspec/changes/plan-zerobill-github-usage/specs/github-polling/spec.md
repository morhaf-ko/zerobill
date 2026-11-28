## ADDED Requirements

### Requirement: Evaluate runner failover every minute

The service SHALL evaluate fresh runner and active workflow queue observations
on the configured one-minute schedule. It SHALL keep the existing queue timeout,
routing hold and recovery safety checks.

#### Scenario: A later job queues in an active workflow

- **WHEN** a job queues in a running workflow whose parent run list is unchanged
- **THEN** the next successful poll observes the job and computes its age from
  its queue timestamp

#### Scenario: A fresh routing decision changes the runner label

- **WHEN** fresh observations make the current provider unavailable and an eligible
  alternative exists
- **THEN** the service reconciles the applicable variables in the current check
  and verifies them before attempting guarded workflow recovery

### Requirement: Bound and measure REST use

The service SHALL measure requests by endpoint class and status. It SHALL
reduce billed REST usage while preserving authorized repository coverage,
conservative hosted capacity decisions and guarded recovery.

#### Scenario: A paginated endpoint returns a matching 304

- **WHEN** a correctly authorized conditional read returns 304
- **THEN** the service reuses the matching response body and pagination metadata
  and records the HTTP attempt independently of charged primary quota

#### Scenario: Hosted capacity is stale or incomplete

- **WHEN** the current billing month's usage cannot be bounded with a complete
  and sufficiently fresh observation
- **THEN** the service does not authorize a new route or rerun using that
  hosted capacity observation

#### Scenario: The existing App cannot provide adequate coverage

- **WHEN** an installation credential cannot cover the required endpoints and
  repositories, or an approved refresh mechanism is unavailable
- **THEN** the reduced-volume design retains the current credential path pending
  a separately approved identity decision
