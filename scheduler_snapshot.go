package main

import (
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type SchedulerSnapshot struct {
	HandleEnabled        bool
	AffinityMode         string
	AffinityTTL          time.Duration
	Fallback             FallbackMode
	MonthlyMode          MonthlyMode
	ResetAwareScheduling bool
	GlobalResetTimes     []time.Time
	Accounts             []AccountView
	ActiveHighestTier    map[string]struct{}
	Trials               *TrialRegistry
	EvidenceIntents      chan<- EvidenceIntent
	AdmissionVersion     uint64
	Activity             func(pluginapi.SchedulerPickRequest, uint64, time.Time)
	Observation          func(pluginapi.SchedulerPickRequest, PickDecision, time.Time)
}

type EvidenceIntent struct {
	AuthID   string
	Instance AuthInstanceID
	BeganAt  time.Time
}

var publishedSchedulerSnapshot atomic.Pointer[SchedulerSnapshot]

func PublishSchedulerSnapshot(snapshot *SchedulerSnapshot) {
	if snapshot == nil {
		return
	}
	copy := cloneSchedulerSnapshot(*snapshot)
	publishedSchedulerSnapshot.Store(&copy)
}
func cloneSchedulerSnapshot(s SchedulerSnapshot) SchedulerSnapshot {
	s.Accounts = append([]AccountView(nil), s.Accounts...)
	s.GlobalResetTimes = append([]time.Time(nil), s.GlobalResetTimes...)
	s.ActiveHighestTier = cloneStringSet(s.ActiveHighestTier)
	return s
}
func cloneStringSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}

func schedulerPickPublished(req pluginapi.SchedulerPickRequest, now time.Time) PickDecision {
	snapshot := publishedSchedulerSnapshot.Load()
	if snapshot == nil {
		return PickDecision{Reason: "handle_disabled"}
	}
	if !requestIncludesCodex(req) || codexCandidateCount(req) == 0 {
		return PickDecision{Reason: "provider_not_codex"}
	}
	if !snapshot.HandleEnabled {
		return observeSchedulerDecision(snapshot, req, PickDecision{Reason: "handle_disabled"}, now)
	}
	if snapshot.Activity != nil {
		snapshot.Activity(req, snapshot.AdmissionVersion, now)
	}
	decision := globalAffinity.pick(*snapshot, req, now)
	return observeSchedulerDecision(snapshot, req, decision, now)
}

func pickSnapshotAccount(snapshot SchedulerSnapshot, req pluginapi.SchedulerPickRequest, now time.Time, preferredID string) (PickDecision, AuthInstanceID) {
	candidates := make([]Candidate, 0, len(req.Candidates))
	for _, c := range req.Candidates {
		candidates = append(candidates, Candidate{ID: c.ID, Provider: c.Provider})
	}
	result := selectAccountWithAffinity(snapshot, candidates, now, nil, snapshot.Trials, preferredID)
	var skipped map[AuthInstanceID]struct{}
	for result.AuthID != "" && result.Class == Opportunistic && (snapshot.Trials == nil || !snapshot.Trials.TryBegin(result.Instance, now)) {
		if skipped == nil {
			skipped = make(map[AuthInstanceID]struct{})
		}
		skipped[result.Instance] = struct{}{}
		result = selectAccountWithAffinity(snapshot, candidates, now, skipped, snapshot.Trials, preferredID)
	}
	if result.AuthID != "" && result.Class == Opportunistic {
		select {
		case snapshot.EvidenceIntents <- EvidenceIntent{AuthID: result.AuthID, Instance: result.Instance, BeganAt: now}:
			snapshot.Trials.MarkEvidencePending(result.Instance, true)
		default:
		}
	}
	if result.AuthID != "" {
		reason := result.Reason
		if preferredID != "" && result.AuthID != preferredID {
			reason = "session_affinity_reselected"
		}
		return PickDecision{AuthID: result.AuthID, Handled: true, Reason: reason}, result.Instance
	}
	// Do not let builtin fallback resurrect a quota/health/trial-excluded account.
	return PickDecision{Handled: true, Reason: "no_selectable_account"}, 0
}

func observeSchedulerDecision(snapshot *SchedulerSnapshot, req pluginapi.SchedulerPickRequest, decision PickDecision, now time.Time) PickDecision {
	if snapshot != nil && snapshot.Observation != nil {
		snapshot.Observation(req, decision, now)
	}
	return decision
}

func schedulerSnapshotFromState(state StateSnapshot, trials *TrialRegistry) *SchedulerSnapshot {
	active := cloneStringSet(state.CPAAdmission.AuthIDs)
	accounts := make([]AccountView, 0, len(state.Accounts))
	for _, a := range state.Accounts {
		accounts = append(accounts, accountViewFromState(a, state.Config, state.Now, trials))
	}
	var activity func(pluginapi.SchedulerPickRequest, uint64, time.Time)
	var observation func(pluginapi.SchedulerPickRequest, PickDecision, time.Time)
	if pump := globalPickActivityPump.Load(); pump != nil {
		activity = pump.enqueue
		observation = pump.enqueueObservation
	}
	return &SchedulerSnapshot{HandleEnabled: state.Config.HandleEnabled, AffinityMode: state.Config.AffinityMode, AffinityTTL: state.Config.AffinityTTL, Fallback: state.Config.Fallback, MonthlyMode: state.Config.MonthlyMode, ResetAwareScheduling: state.Config.ResetAwareScheduling, GlobalResetTimes: append([]time.Time(nil), state.Config.GlobalResetTimes...), Accounts: accounts, ActiveHighestTier: active, Trials: trials, EvidenceIntents: globalEvidenceIntents, Activity: activity, Observation: observation}
}

func accountViewFromState(a AccountState, cfg Config, now time.Time, trials *TrialRegistry) AccountView {
	cache := CacheFresh
	if a.LastSuccessAt.IsZero() {
		cache = CacheUnknown
	} else if a.Stale {
		cache = CacheStale
	} else if now.Sub(a.LastSuccessAt) > cfg.QuotaRefreshInterval {
		cache = CacheAging
	}
	exhausted, reset := accountExhaustion(a, now)
	trial := TrialNone
	if trials != nil {
		trial = trials.State(a.Instance, now)
	}
	circuit := effectiveCircuitState(a.Circuit, now).EffectiveState
	circuitClass := CircuitClosed
	if circuit == CircuitStateOpen {
		circuitClass = CircuitOpen
	} else if circuit == CircuitStateHalfOpen {
		circuitClass = CircuitHalfOpen
	}
	return AccountView{
		resetAwareInput: resetPolicyState(a, now),
		ID:              a.AuthID, AuthIndex: a.AuthIndex, Instance: a.Instance,
		PluginPriority: a.Annotation.SchedulerPriority, CPAPriority: a.Priority, Family: a.Family,
		Cache: cache, LastKnownAvailable: a.LastError == "", Exhausted: exhausted,
		ResetAt: reset, AuthBlocked: a.Refresh.AuthFailure, Circuit: circuitClass,
		TemporaryUnavailable: a.TemporaryExhausted && a.TemporaryResetAt.After(now),
		Trial:                trial, Expiry: accountSortTime(a), RemainingQuota: remainingQuota(a), QuotaPressure: quotaPressure(a, now),
	}
}

func publishSchedulerState(state *PluginState, active map[string]struct{}, now time.Time) {
	if state == nil {
		return
	}
	s := state.Snapshot(now)
	if active != nil {
		s.CPAAdmission = CPAAdmissionState{Observed: true, AuthIDs: cloneStringSet(active)}
	}
	snapshot := schedulerSnapshotFromState(s, globalTrials)
	_, snapshot.AdmissionVersion = state.CPAAdmissionVersioned()
	PublishSchedulerSnapshot(snapshot)
}
func accountExhaustion(a AccountState, now time.Time) (bool, time.Time) {
	if windowExhausted(a.Quota.LongWindow, now) {
		return true, a.Quota.LongWindow.ResetAt
	}
	if windowExhausted(a.Quota.FiveHour, now) {
		return true, a.Quota.FiveHour.ResetAt
	}
	return false, time.Time{}
}
func remainingQuota(a AccountState) float64 {
	for _, w := range []*QuotaWindow{a.Quota.LongWindow, a.Quota.FiveHour} {
		if w != nil && w.UsedPercent != nil {
			return 100 - *w.UsedPercent
		}
	}
	return 0
}

const minimumQuotaPressureWindow = 30 * time.Minute

// quotaPressure estimates how quickly the remaining long-window quota must be
// consumed before reset. A 30-minute floor keeps the score bounded close to a
// reset. Unknown long-window usage has no pressure and falls through to the
// deterministic expiry/remaining-quota tie breakers.
func quotaPressure(a AccountState, now time.Time) float64 {
	window := a.Quota.LongWindow
	if window == nil || window.UsedPercent == nil || window.ResetAt.IsZero() {
		return 0
	}
	untilReset := window.ResetAt.Sub(now)
	if untilReset <= 0 {
		return 0
	}
	if untilReset < minimumQuotaPressureWindow {
		untilReset = minimumQuotaPressureWindow
	}
	remaining := 100 - *window.UsedPercent
	if remaining < 0 {
		remaining = 0
	} else if remaining > 100 {
		remaining = 100
	}
	return remaining / untilReset.Hours()
}
