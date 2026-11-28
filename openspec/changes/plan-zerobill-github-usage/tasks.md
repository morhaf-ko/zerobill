## 1. Plan evidence

- [x] 1.1 Count v0.6.0 calls by endpoint and distinguish source estimates, measured production ticks and shared quota. See the production evidence and endpoint inventory in [design.md](design.md).
- [x] 1.2 Compare conditional polling, events, change detection, slower intervals, GraphQL and duplicate removal against GitHub's API documentation. See the endpoint inventory, rollout order and webhook and GraphQL assessment in [design.md](design.md).
- [x] 1.3 Evaluate the existing App installation, identity and permission boundaries, and operator steps. See installation identity in [design.md](design.md).
- [x] 1.4 Specify staged request budgets, failure behavior and one-minute failover acceptance. See the model and cache behavior in [design.md](design.md) and the [proposed specification](specs/github-polling/spec.md).

## 2. Implementation

- [ ] 2.1 Instrument endpoint counts and measure comparable quiet and busy windows.
- [x] 2.2 Add shared inventory, workflow SHA reuse and per-variable reconciliation, with partial-write and drift tests. See [verification](verification.md).
- [x] 2.3 Verify conservative capacity refresh and preserve failover during cold, quota-limited and month-rollover conditions. Keep the fresh-read fallback where usage cannot be bounded. See [verification](verification.md).
- [ ] 2.4 Verify conditional pagination, workload savings and end-to-end failover and guarded recovery.
- [x] 2.5 Evaluate App migration and optional event delivery against the design gates. See [verification](verification.md).
