// Experimental opt-in reset-aware policy adapter. Existing admission,
// priorities, trial handling, health state, and probes remain authoritative.
package main

import (
	"log"
	"os"
	"sync"
	"time"

	"github.com/jeffery/codex-quota-scheduler/internal/resetpolicy"
)

type resetAwareInput = resetpolicy.Account

type resetAwareKey struct {
	enabled bool
	key     resetpolicy.Key
}

var resetPolicyFile resetpolicy.FileLoader
var resetPolicyError struct {
	sync.Mutex
	message string
}

func resetPolicyOptions(now time.Time) resetpolicy.FileOptions {
	o, err := resetPolicyFile.Load(os.Getenv("CPA_RESET_POLICY_FILE"), now)
	message := ""
	if err != nil {
		message = err.Error()
	}

	resetPolicyError.Lock()
	if message != resetPolicyError.message {
		if message != "" {
			log.Printf("reset-aware addon disabled: invalid or unreadable policy configuration")
		}
		resetPolicyError.message = message
	}
	resetPolicyError.Unlock()
	return o
}

func resetPolicyWindow(w *QuotaWindow, defaultPeriod time.Duration, now time.Time) resetpolicy.Window {
	if w == nil {
		return resetpolicy.Window{}
	}
	out := resetpolicy.Window{
		Period:  defaultPeriod,
		ResetAt: w.ResetAt,
		Running: w.ResetAt.After(now),
	}
	if w.UsedPercent != nil {
		out.Known = true
		out.Remaining = 1 - *w.UsedPercent/100
	}
	if w.LimitWindowSeconds != nil && *w.LimitWindowSeconds > 0 && *w.LimitWindowSeconds <= 31536000 {
		out.Period = time.Duration(*w.LimitWindowSeconds) * time.Second
	}
	return out
}

func resetPolicyState(a AccountState, now time.Time) resetAwareInput {
	return resetAwareInput{
		ID:       a.AuthID,
		Eligible: true,
		Weekly:   resetPolicyWindow(a.Quota.LongWindow, 0, now),
		HasShort: a.Quota.FiveHour != nil,
		Short:    resetPolicyWindow(a.Quota.FiveHour, 5*time.Hour, now),
	}
}

func applyResetAwarePolicy(accounts []AccountView, now time.Time) []AccountView {
	o := resetPolicyOptions(now)
	if !o.Enabled || len(accounts) == 0 {
		return accounts
	}

	inputs := make([]resetpolicy.Account, len(accounts))
	for i, a := range accounts {
		input := a.resetAwareInput
		calibration := o.Accounts[a.ID]
		input.CapacityWeight = calibration.CapacityWeight
		input.LongToShort = calibration.LongToShort
		input.AssignedFractionPerHour = calibration.AssignedFractionPerHour
		inputs[i] = input
	}

	keys := resetpolicy.Keys(inputs, o.PolicyConfig(), now)
	for i := range accounts {
		accounts[i].resetAwareRank = resetAwareKey{enabled: true, key: keys[i]}
	}
	return accounts
}

func resetAwareLess(a, b AccountView) (less, decided bool) {
	if !a.resetAwareRank.enabled || !b.resetAwareRank.enabled {
		return false, false
	}
	comparison := resetpolicy.Compare(a.resetAwareRank.key, b.resetAwareRank.key)
	return comparison < 0, comparison != 0
}
