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

Account binding is owned entirely by this plugin, using CPA main's standard
scheduler, before/after-auth interceptors, and request-completion callbacks.
No custom CPA capability, host-affinity callback, or host patch is required.
The build still uses the published v7.3.8 SDK with no local module replacement.

```yaml
codex-quota-scheduler:
  enabled: true
  handle_enabled: true
  affinity_mode: fill-first
  affinity_ttl: 4h
  schedule_across_priorities: true
  reset_aware_scheduling: true
```

`affinity_mode` defaults to `fill-first`; `off` restores ranking on every request.
`affinity_ttl` is a positive duration, default `4h`. Both are available in
Scheduler Settings. Existing persisted configurations acquire these defaults.
CPA does not expose its built-in routing settings to scheduler plugins, so these
plugin settings are independent of `routing.strategy` and
`routing.session-affinity`. Disabling plugin takeover restores CPA's own routing.

The host provides its canonical explicit identity for header/body sessions,
prompt-cache keys, and execution sessions. Otherwise the plugin hashes caller
scope, the normalized provider pool, and the base model into a default binding.
Changing request content does not change that default binding. Unknown callers
without an explicit identity are not merged. Independent tasks should supply
independent explicit IDs. Prompt bodies and upstream conversation IDs are never
rewritten, and no credentials or raw session identities are stored in the cache.

A usable binding wins over CPA priority, account `scheduler_priority`, quota
pressure, and reset-aware scores. Ranking runs for new or expired bindings and
when the bound account fails host candidate checks or plugin quota, health,
roster-instance, or trial checks. A failed A followed by a successful B stays on B
when A recovers. The host's retry candidate exclusions still apply. No eligible
account returns HTTP 503; builtin fallback cannot select an excluded account.
After a cold start, roster and quota discovery may need to finish before an
account becomes eligible.

Selection and binding updates are serialized in the plugin. Every selection
gets a generation, so a late failure or success cannot remove or restore a newer
binding, including an A-to-B-to-A transition. Standard request-completion events
refresh successful bindings and remove matching credential-failure bindings;
cancellation, rejection, and request-scoped errors preserve them. A temporary
random header correlates standard before-auth and scheduler calls, and the
after-auth interceptor clears its value before execution. Main retains an empty
header marker internally; its Codex executor does not forward that marker. The cache is
bounded and in-memory; configuration changes preserve bindings, while plugin
unload or process restart clears them. Direct SDK scheduler calls without request
hooks still reuse bindings and enforce candidate availability, but cannot receive
per-request completion updates.

The Account Queue previews new allocations and failover. Existing bindings can
continue using an account farther down the queue. Logs distinguish
`session_affinity` from `session_affinity_reselected`. The available-only filter
does not alter scheduling. The Home dispatch path remains managed by Home.

## Main compatibility and stream behavior

The native plugin integration test uses unmodified official CPA main
`ffe6ad3c` and exercises real scheduler/interceptor ABI calls, main's session
normalization, non-streaming and streaming request handlers, cross-priority
failover, caller isolation, and clearing of the private correlation token:

```bash
bash scripts/test-main-affinity.sh /path/to/CPA-main
```

The script uses a Go test overlay, synthetic credentials, an HTTP transport that
never opens a connection, and a state directory isolated before process startup.
It does not edit CPA source or touch a running service.

Official main at that commit does **not** implement the quota branch's
`codex.stream-full-buffering`. That setting has no effect on main. This account
binding change does not add full-response buffering or replay already-delivered
output. Main retains its own stream commitment and retry rules; bootstrap
buffering is not equivalent to full-response buffering. Migrating the latter
requires a separate plugin execution/buffering change.
