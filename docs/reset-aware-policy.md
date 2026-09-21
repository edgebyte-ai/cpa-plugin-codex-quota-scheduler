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

This change does not implement a second job-affinity layer. In CPA v7.3.8's
legacy non-Home path the plugin scheduler runs before the built-in selector, so
built-in selector affinity is not guaranteed to run first. If an upper layer
pins jobs to credentials before the plugin is called, keep that mechanism;
otherwise affinity is a separate concern from this ranking policy.
