package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func affinityPickRequest(preferred string) pluginapi.SchedulerPickRequest {
	return pluginapi.SchedulerPickRequest{Provider: "codex", Options: pluginapi.SchedulerOptions{Metadata: map[string]any{
		schedulerAffinityEnabledKey: true, schedulerAffinityAuthIDKey: preferred,
	}}, Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "A", Provider: "codex"}, {ID: "B", Provider: "codex"}}}
}

func TestSchedulerAffinityOverridesResetRanking(t *testing.T) {
	previous := publishedSchedulerSnapshot.Load()
	t.Cleanup(func() { publishedSchedulerSnapshot.Store(previous) })
	enableResetPolicyForTest(t)
	now, s, _ := resetPolicyFixture(t)
	s.HandleEnabled = true
	PublishSchedulerSnapshot(&s)
	if got := schedulerPickPublished(affinityPickRequest(""), now); got.AuthID != "A" {
		t.Fatalf("new binding = %+v", got)
	}
	if got := schedulerPickPublished(affinityPickRequest("B"), now); got.AuthID != "B" || got.Reason != "session_affinity" {
		t.Fatalf("bound account = %+v", got)
	}
	s.Accounts[0].CPAPriority = 99
	s.Accounts[0].PluginPriority = 99
	PublishSchedulerSnapshot(&s)
	if got := schedulerPickPublished(affinityPickRequest("B"), now); got.AuthID != "B" {
		t.Fatalf("priority preempted binding: %+v", got)
	}
	for _, block := range []string{"five-hour", "auth", "circuit", "temporary", "trial", "roster", "candidate"} {
		t.Run(block, func(t *testing.T) {
			snapshot := cloneSchedulerSnapshot(s)
			req := affinityPickRequest("B")
			switch block {
			case "five-hour":
				snapshot.Accounts[1].Exhausted = true
				snapshot.Accounts[1].ResetAt = now.Add(time.Hour)
			case "auth":
				snapshot.Accounts[1].AuthBlocked = true
			case "circuit":
				snapshot.Accounts[1].Circuit = CircuitOpen
			case "temporary":
				snapshot.Accounts[1].TemporaryUnavailable = true
			case "trial":
				snapshot.Accounts[1].Trial = TrialActive
			case "roster":
				delete(snapshot.ActiveHighestTier, "B")
			case "candidate":
				req.Candidates = req.Candidates[:1]
			}
			PublishSchedulerSnapshot(&snapshot)
			got := schedulerPickPublished(req, now)
			if got.AuthID != "A" || got.Reason != "session_affinity_reselected" {
				t.Fatalf("unavailable binding reused: %+v", got)
			}
		})
	}
}

func TestSchedulerAffinityABIUnavailableDoesNotDelegate(t *testing.T) {
	previous := publishedSchedulerSnapshot.Load()
	t.Cleanup(func() { publishedSchedulerSnapshot.Store(previous) })
	PublishSchedulerSnapshot(&SchedulerSnapshot{HandleEnabled: true, Fallback: FallbackFillFirst,
		Accounts: []AccountView{{ID: "A", Cache: CacheFresh, AuthBlocked: true}}, ActiveHighestTier: map[string]struct{}{"A": {}},
	})
	raw, _ := json.Marshal(affinityPickRequest("A"))
	encoded, err := handleSchedulerPick(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(encoded, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil || env.Error.HTTPStatus != 503 || env.Error.Code != "auth_unavailable" {
		t.Fatalf("unexpected response: %s", encoded)
	}
	// Older hosts retain their documented fallback contract.
	req := affinityPickRequest("A")
	req.Options.Metadata = nil
	if got := schedulerPickPublished(req, time.Now()); got.DelegateBuiltin != pluginapi.SchedulerBuiltinFillFirst {
		t.Fatalf("legacy fallback changed: %+v", got)
	}
	if !PluginRegistration().Capabilities.SchedulerSessionAffinity {
		t.Fatal("affinity capability not registered")
	}
}

func TestRosterControllerAndQuotaObservationKeepLowerPriorities(t *testing.T) {
	previousSnapshot := publishedSchedulerSnapshot.Load()
	t.Cleanup(func() {
		publishedSchedulerSnapshot.Store(previousSnapshot)
	})
	now := time.Now()
	high, low := 9, 1
	across := true
	host := &rosterTestHost{entries: []RosterEntry{{ID: "high", Provider: "codex", Priority: &high}, {ID: "low", Provider: "codex", Priority: &low}}}
	controller := NewRosterController(RosterControllerOptions{Host: host, Now: func() time.Time { return now }, AcrossPriorities: func() bool { return across }})
	active, err := controller.Startup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(active.Instances, []string{"high", "low"}) || len(active.Entries) != 2 {
		t.Fatalf("roster lost lower tier: %+v", active)
	}
	store := NewPluginState(DefaultConfig())
	for _, account := range []AccountState{weeklyAccount("high", high, now.Add(24*time.Hour), true), weeklyAccount("low", low, now.Add(24*time.Hour), false)} {
		account.LastSuccessAt = now
		store.UpsertQuota(account)
	}
	roster := hostRosterSnapshotFromActive(active)
	refresher := &QuotaRefresher{state: store, roster: roster, now: func() time.Time { return now }}
	refresher.publishSchedulerStateFromRoster()
	snapshot := publishedSchedulerSnapshot.Load()
	if _, ok := snapshot.ActiveHighestTier["low"]; !ok {
		t.Fatal("quota observation removed lower tier")
	}
	if got := SelectAccount(*snapshot, []Candidate{{ID: "high", Provider: "codex"}, {ID: "low", Provider: "codex"}}, now); got.AuthID != "low" {
		t.Fatalf("exhausted high tier blocked failover: %+v", got)
	}
	across = false
	active, err = controller.OnSyncResult(context.Background(), host.entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(active.Instances, []string{"high"}) {
		t.Fatalf("legacy single-tier roster changed: %+v", active)
	}
}
