package main

import (
	"context"
	"reflect"
	"testing"
	"time"
)

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
