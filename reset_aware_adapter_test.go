package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func resetPolicyFixture(t *testing.T) (time.Time, SchedulerSnapshot, []Candidate) {
	t.Helper()
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	zero := 0.0
	a := AccountState{AuthID:"A", Quota:ParsedQuota{
		LongWindow:&QuotaWindow{UsedPercent:&zero, ResetAt:now.Add(168*time.Hour)},
		FiveHour:&QuotaWindow{UsedPercent:&zero, ResetAt:now.Add(5*time.Hour)},
	}}
	b := AccountState{AuthID:"B", Quota:ParsedQuota{
		LongWindow:&QuotaWindow{UsedPercent:&zero, ResetAt:now.Add(24*time.Hour)},
	}}
	s := SchedulerSnapshot{
		Accounts:[]AccountView{
			{ID:"A", Instance:1, Cache:CacheFresh, QuotaPressure:.01, resetAwareInput:resetPolicyState(a,now)},
			{ID:"B", Instance:2, Cache:CacheFresh, QuotaPressure:.1, resetAwareInput:resetPolicyState(b,now)},
		},
		ActiveHighestTier:map[string]struct{}{"A":{},"B":{}},
	}
	return now,s,[]Candidate{{ID:"A",Provider:"codex"},{ID:"B",Provider:"codex"}}
}

func enableResetPolicyForTest(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(),"policy.json")
	body := `{"enabled":true,"global_reset_at":"2026-09-20T10:00:00Z","accounts":{"A":{"long_to_short_ratio":10}}}`
	if err := os.WriteFile(path,[]byte(body),0600); err != nil { t.Fatal(err) }
	t.Setenv("CPA_RESET_POLICY_FILE",path)
}

func TestResetAddonDisabledPreservesOriginalSelection(t *testing.T) {
	t.Setenv("CPA_RESET_POLICY_FILE","")
	now,s,cs := resetPolicyFixture(t)
	if got := SelectAccount(s,cs,now); got.AuthID != "B" {
		t.Fatalf("disabled addon changed selection: %+v",got)
	}
}

func TestResetAddonUsesGlobalReset(t *testing.T) {
	enableResetPolicyForTest(t)
	now,s,cs := resetPolicyFixture(t)
	if got := SelectAccount(s,cs,now); got.AuthID != "A" {
		t.Fatalf("reset-aware choice missing: %+v",got)
	}
}

func TestResetAddonPreservesHigherPriority(t *testing.T) {
	enableResetPolicyForTest(t)
	now,s,cs := resetPolicyFixture(t)
	s.Accounts[1].CPAPriority = 10
	if got := SelectAccount(s,cs,now); got.AuthID != "B" {
		t.Fatalf("CPA priority overridden: %+v",got)
	}
}
