package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestGlobalResetTimesNormalizeSortAndDeduplicate(t *testing.T) {
	a := time.Date(2026,9,22,10,0,0,0,time.UTC)
	b := time.Date(2026,9,21,10,0,0,0,time.UTC)
	cfg := NormalizeConfig(Config{GlobalResetTimes: []time.Time{a,b,a}})
	if len(cfg.GlobalResetTimes) != 2 || !cfg.GlobalResetTimes[0].Equal(b) || !cfg.GlobalResetTimes[1].Equal(a) {
		t.Fatalf("normalized resets = %#v", cfg.GlobalResetTimes)
	}
}

func TestSettingsAcceptMultipleGlobalResets(t *testing.T) {
	base := DefaultConfig()
	a := time.Date(2026,9,21,10,0,0,0,time.UTC)
	b := time.Date(2026,9,28,10,0,0,0,time.UTC)
	payload := SettingsFromConfig(base)
	payload.ResetAwareScheduling = true
	payload.GlobalResetTimes = []time.Time{b,a}
	cfg, err := ConfigFromSettings(base, payload)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ResetAwareScheduling || len(cfg.GlobalResetTimes) != 2 || !cfg.GlobalResetTimes[0].Equal(a) {
		t.Fatalf("cfg = %#v", cfg)
	}
}

func TestSettingsRejectTooManyGlobalResets(t *testing.T) {
	payload := SettingsFromConfig(DefaultConfig())
	payload.GlobalResetTimes = make([]time.Time, 257)
	for i := range payload.GlobalResetTimes {
		payload.GlobalResetTimes[i] = time.Unix(int64(i+1),0).UTC()
	}
	if _, err := ConfigFromSettings(DefaultConfig(), payload); err == nil {
		t.Fatal("expected reset list limit error")
	}
}

func TestSettingsSavePublishesResetPolicyWithoutRestart(t *testing.T) {
	now := time.Date(2026,9,20,12,0,0,0,time.UTC)
	store := NewPluginState(DefaultConfig())
	payload := SettingsFromConfig(DefaultConfig())
	payload.ResetAwareScheduling = true
	payload.GlobalResetTimes = []time.Time{now.Add(2*time.Hour)}
	body, _ := json.Marshal(payload)
	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{Method:http.MethodPut, Path:"/settings", Body:body}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save status=%d body=%s", resp.StatusCode, resp.Body)
	}
	snapshot := publishedSchedulerSnapshot.Load()
	if snapshot == nil || !snapshot.ResetAwareScheduling || len(snapshot.GlobalResetTimes) != 1 {
		t.Fatalf("published snapshot = %#v", snapshot)
	}
}

func TestManagementUIRendersResetFields(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{Method:http.MethodGet, Path:"/status"}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	html := string(resp.Body)
	for _, id := range []string{`id="resetAwareScheduling"`, `id="globalResetTimes"`, `id="addGlobalResetTime"`, `type="datetime-local"`} {
		if !strings.Contains(html,id) {
			t.Fatalf("missing UI field %s", id)
		}
	}
}


func TestManagementUIConvertsLocalResetTimeToISO(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{Method:http.MethodGet, Path:"/status"}, time.Now())
	html := string(resp.Body)
	for _, snippet := range []string{
		"function globalResetISO(value)",
		"date.toISOString()",
		"Intl.DateTimeFormat().resolvedOptions().timeZone",
		"function collectGlobalResetTimes()",
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("missing local-time conversion logic %q", snippet)
		}
	}
}
