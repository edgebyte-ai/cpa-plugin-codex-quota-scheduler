# Reset-aware scheduling policy

This branch adds an **opt-in** account-ranking policy on top of the existing
eligibility, priority, health, circuit-breaker, trial, and fallback logic.

## Management UI and hot reload

Open **Codex Scheduler → Scheduler Settings** and enable **Reset-aware
scheduling**. Enter all-account reset timestamps in the **All-account reset
times** field, one ISO-8601 timestamp per line, for example:

```text
2026-09-21T10:00:00-07:00
2026-09-28T10:00:00-07:00
2026-10-05T10:00:00-07:00
```

Up to 256 timestamps are accepted. They are sorted and deduplicated when saved.
Past timestamps are ignored by the scheduler. At every pick, the nearest future
all-account reset is compared with each account's natural long-window reset;
the earlier timestamp becomes the effective deadline.

Saving the Settings form persists the values in the plugin user-data file,
updates the in-memory configuration, and immediately republishes the scheduler
snapshot. **CPA does not need to be restarted.** Importing a configuration also
republishes the snapshot immediately.

The setting is disabled by default.

## Scheduling behavior

The policy uses the earlier of the account's observed long-window reset and the
nearest known future all-account reset as the effective deadline. When a
reliable long-to-short capacity ratio is supplied, it also projects how much
long-window credit can still be reached through remaining 5-hour windows before
that deadline.

The optional `CPA_RESET_POLICY_FILE` remains available for advanced calibration
inputs such as `long_to_short_ratio`, `capacity_weight`, and assigned-load
forecasts. UI-managed enablement and reset timestamps do not require that file.

`capacity_weight` is only used when calibrated-capacity mode is explicitly
enabled in the advanced file. All usable accounts must then have positive
weights; otherwise the comparison falls back to percentage units. Do not infer
cross-account weights from arbitrary tasks' percentage drops unless task costs
are independently comparable.

This policy never fabricates a refill, restarts a quota timer, changes account
health, or bypasses upstream eligibility. A reset timestamp is a scheduling
deadline, not proof that a reset happened; actual availability still comes from
quota observations.

## Affinity

The plugin declares the additive `scheduler_session_affinity` capability. On a
CPA host supporting that capability, CPA owns session identity, binding expiry,
and execution-result tracking. The plugin validates the bound account against
its quota, health, roster, and trial rules before considering any ranking.

A usable binding wins over CPA priority, account `scheduler_priority`, quota
pressure, and reset-aware scores. Ranking runs for a new binding, an expired
binding, or a bound account that is no longer eligible. If A fails and B succeeds,
later requests continue on B even when A recovers. If every candidate is excluded,
the plugin returns HTTP 503 rather than delegating to a builtin selector that
could select an excluded account.

With CPA's fill-first strategy, requests without an explicit session use CPA's
caller/API-key, provider-pool, and normalized-model default binding. Explicit
session IDs remain authoritative. The default binding is routing-only and does
not change the upstream conversation or prompt-cache identity. Distinct jobs
requiring independent allocations should supply distinct session IDs.

The Account Queue previews new allocations and failover; it is not a global
prediction for requests already bound to an account. The available-only filter
does not alter scheduling. Logs distinguish `session_affinity` reuse from
`session_affinity_reselected` failover. Stream full buffering and credential
retry remain owned by CPA.

This requires `routing.session-affinity: true` and the matching CPA change on
`codex/codex-quota-probe`. Update both
CPA and the plugin; rebuilding only the plugin on an older host does not enable
binding integration. Older hosts ignore the capability and retain legacy
scheduling. The Home dispatch path remains managed by Home.

The wire contract uses `SchedulerOptions.Metadata`: the host overwrites
`scheduler_session_affinity` (boolean) and `scheduler_affinity_auth_id` (string,
empty on a miss) on each pick. The preferred ID must also be in `Candidates`.
This keeps the plugin build compatible with the v7.3.8 SDK without a local
absolute-path module replacement.
