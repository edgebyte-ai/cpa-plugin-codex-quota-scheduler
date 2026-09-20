# Reset-aware scheduling policy

This branch adds an **opt-in** account-ranking policy on top of the existing
eligibility, priority, health, circuit-breaker, trial, and fallback logic.

Set `CPA_RESET_POLICY_FILE` to a JSON file to enable it. With the variable
unset, selection behavior is unchanged.

Example:

```json
{
  "enabled": true,
  "global_reset_at": "2026-09-21T10:00:00-07:00",
  "deadline_floor_seconds": 60,
  "activation_delay_seconds": 1,
  "calibrated_capacity": false,
  "accounts": {
    "auth-id-A": {
      "long_to_short_ratio": 10
    }
  }
}
```

The policy uses the earlier of the account's observed long-window reset and
`global_reset_at` as the effective deadline. When a reliable long-to-short
capacity ratio is supplied, it also projects how much long-window credit can
still be reached through remaining 5-hour windows before that deadline.

`capacity_weight` is optional and only used when `calibrated_capacity=true`.
All usable accounts must then have positive weights; otherwise the comparison
falls back to percentage units. Do not infer cross-account weights from
arbitrary tasks' percentage drops unless task costs are independently
comparable.

This policy never fabricates a refill, restarts a quota timer, changes account
health, or bypasses upstream eligibility. Passed global-reset forecasts are
ignored until real quota observations confirm the reset.

## Affinity

This change does not implement a second job-affinity layer. In CPA v7.3.8's
legacy non-Home path the plugin scheduler runs before the built-in selector, so
built-in selector affinity is not guaranteed to run first. If an upper layer
pins jobs to credentials before the plugin is called, keep that mechanism;
otherwise affinity is a separate concern from this ranking policy.
