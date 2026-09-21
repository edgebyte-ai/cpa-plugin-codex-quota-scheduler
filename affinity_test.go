package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func affinityTestRequest(caller, session string) pluginapi.SchedulerPickRequest {
	return pluginapi.SchedulerPickRequest{Provider: "codex", Providers: []string{"codex"}, Model: "gpt-test(high)",
		Options:    pluginapi.SchedulerOptions{Metadata: map[string]any{"caller_scope": caller, "canonical_session_id": session}},
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "A", Provider: "codex"}, {ID: "B", Provider: "codex"}},
	}
}

func TestPluginOwnedAffinityRankingAndScope(t *testing.T) {
	enableResetPolicyForTest(t)
	now, s, _ := resetPolicyFixture(t)
	c := newAffinityCache(32)
	s.HandleEnabled = true
	req := affinityTestRequest("caller", "")
	first := req
	first.Candidates = first.Candidates[1:]
	if got := c.pick(s, first, now); got.AuthID != "B" {
		t.Fatal(got)
	}
	if got := c.pick(s, req, now); got.AuthID != "B" || got.Reason != "session_affinity" {
		t.Fatal(got)
	}
	// A is the reset-aware winner; independent scopes receive new allocations.
	for _, field := range []string{"caller", "model", "pool", "session"} {
		t.Run(field, func(t *testing.T) {
			r := affinityTestRequest("caller", "")
			switch field {
			case "caller":
				r.Options.Metadata["caller_scope"] = "other"
			case "model":
				r.Model = "other"
			case "pool":
				r.Providers = []string{"codex", "other"}
			case "session":
				r.Options.Metadata["canonical_session_id"] = "codex:explicit"
			}
			if got := c.pick(s, r, now); got.AuthID != "A" {
				t.Fatal(got)
			}
		})
	}
	req.Model = "gpt-test"
	req.Options.Metadata["derived_session_id"] = "different-body-every-time"
	s.Accounts[0].CPAPriority, s.Accounts[0].PluginPriority = 99, 99
	if got := c.pick(s, req, now); got.AuthID != "B" {
		t.Fatal(got)
	}
	s.Accounts[1].Exhausted, s.Accounts[1].ResetAt = true, now.Add(time.Hour)
	if got := c.pick(s, req, now); got.AuthID != "A" || got.Reason != "session_affinity_reselected" {
		t.Fatal(got)
	}
	s.Accounts[1].Exhausted = false
	s.Accounts[1].CPAPriority = 100
	if got := c.pick(s, req, now); got.AuthID != "A" {
		t.Fatal(got)
	}
	s.AffinityMode = "off"
	if got := c.pick(s, req, now); got.AuthID != "B" {
		t.Fatal(got)
	}
}

func TestPluginOwnedAffinityEligibility(t *testing.T) {
	t.Setenv("CPA_RESET_POLICY_FILE", "")
	now, s, _ := resetPolicyFixture(t)
	for _, blocked := range []string{"quota", "auth", "circuit", "temporary", "trial", "roster", "candidate", "instance"} {
		t.Run(blocked, func(t *testing.T) {
			c := newAffinityCache(8)
			snapshot := cloneSchedulerSnapshot(s)
			req := affinityTestRequest("caller", "")
			if got := c.pick(snapshot, req, now); got.AuthID != "B" {
				t.Fatal(got)
			}
			switch blocked {
			case "quota":
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
			case "instance":
				snapshot.Accounts[1].Instance = 99
				snapshot.Accounts[0].CPAPriority = 99
			}
			if got := c.pick(snapshot, req, now); got.AuthID != "A" {
				t.Fatal(got)
			}
		})
	}
}

func TestPluginOwnedAffinityResultsAreGenerationGuarded(t *testing.T) {
	t.Setenv("CPA_RESET_POLICY_FILE", "")
	now, s, _ := resetPolicyFixture(t)
	for _, outcome := range []pluginapi.RequestCompletionOutcome{pluginapi.RequestCompletionFailed, pluginapi.RequestCompletionSucceeded, pluginapi.RequestCompletionCanceled, pluginapi.RequestCompletionRejected} {
		t.Run(string(outcome), func(t *testing.T) {
			c := newAffinityCache(8)
			req := affinityTestRequest("caller", "")
			start := func(id string, want string) {
				t.Helper()
				token := c.before(pluginapi.RequestInterceptRequest{RequestID: id}, now)
				req.Options.Headers = http.Header{affinityRequestHeader: []string{token}}
				if got := c.pick(s, req, now); got.AuthID != want {
					t.Fatal(got)
				}
				c.after(pluginapi.RequestInterceptRequest{RequestID: id, Metadata: map[string]any{"selected_auth_id": want}})
			}
			start("old", "B")
			req.Candidates = req.Candidates[:1]
			start("new", "A")
			req.Candidates = affinityTestRequest("caller", "").Candidates
			c.complete(pluginapi.RequestCompletion{RequestID: "old", Outcome: outcome, StatusCode: 503}, now)
			if got := c.pick(s, req, now); got.AuthID != "A" {
				t.Fatalf("late result replaced new binding: %+v", got)
			}
		})
	}
	for _, status := range []int{400, 401, 403, 429, 503} {
		c := newAffinityCache(8)
		req := affinityTestRequest("caller", "")
		token := c.before(pluginapi.RequestInterceptRequest{RequestID: "result"}, now)
		req.Options.Headers = http.Header{affinityRequestHeader: []string{token}}
		c.pick(s, req, now)
		c.after(pluginapi.RequestInterceptRequest{RequestID: "result", Metadata: map[string]any{"selected_auth_id": "B"}})
		c.complete(pluginapi.RequestCompletion{RequestID: "result", Outcome: pluginapi.RequestCompletionFailed, StatusCode: status}, now)
		_, bound := c.bindings[affinityKey(req, affinityProviders(req))]
		if bound == credentialFailureStatus(status) {
			t.Fatalf("status %d: bound=%v", status, bound)
		}
	}
}

func TestPluginOwnedAffinityTTLBoundsAndConcurrentPicks(t *testing.T) {
	t.Setenv("CPA_RESET_POLICY_FILE", "")
	now, s, _ := resetPolicyFixture(t)
	s.AffinityTTL = time.Minute
	c := newAffinityCache(2)
	req := affinityTestRequest("caller", "")
	c.pick(s, req, now)
	s.Accounts[0].CPAPriority = 9
	if got := c.pick(s, req, now.Add(2*time.Minute)); got.AuthID != "A" {
		t.Fatal(got)
	}
	for _, caller := range []string{"one", "two", "three"} {
		c.pick(s, affinityTestRequest(caller, ""), now)
		c.before(pluginapi.RequestInterceptRequest{RequestID: caller}, now)
	}
	if len(c.bindings) != 2 || len(c.requests) != 2 || len(c.requestIDs) != 2 {
		t.Fatal("cache capacity exceeded")
	}
	for key := range c.bindings {
		if len(key) != 64 || strings.Contains(key, "caller") {
			t.Fatal("unhashed identity retained")
		}
	}
	c = newAffinityCache(128)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := c.pick(s, req, now); got.AuthID != "A" {
				t.Errorf("concurrent pick: %+v", got)
			}
		}()
	}
	wg.Wait()
}

func TestPluginOwnedAffinityRetryPoolAndNoCaller(t *testing.T) {
	t.Setenv("CPA_RESET_POLICY_FILE", "")
	now, s, _ := resetPolicyFixture(t)
	c := newAffinityCache(8)
	req := affinityTestRequest("caller", "")
	req.Providers = []string{"other", "codex", "codex"}
	token := c.before(pluginapi.RequestInterceptRequest{RequestID: "retry"}, now)
	req.Options.Headers = http.Header{affinityRequestHeader: []string{token}}
	c.pick(s, req, now)
	req.Providers = []string{"codex"}
	req.Candidates = req.Candidates[:1]
	if got := c.pick(s, req, now); got.AuthID != "A" {
		t.Fatal(got)
	}
	req = affinityTestRequest("caller", "")
	req.Providers = []string{"codex", "other"}
	if got := c.pick(s, req, now); got.AuthID != "A" {
		t.Fatal("provider pool changed mid-retry")
	}
	anonymous := affinityTestRequest("", "")
	if affinityKey(anonymous, affinityProviders(anonymous)) != "" {
		t.Fatal("anonymous defaults must not share a binding")
	}
}

func TestPluginOwnedAffinityABIRejectsUnavailable(t *testing.T) {
	previous := publishedSchedulerSnapshot.Load()
	t.Cleanup(func() { publishedSchedulerSnapshot.Store(previous) })
	PublishSchedulerSnapshot(&SchedulerSnapshot{HandleEnabled: true, Fallback: FallbackFillFirst, Accounts: []AccountView{{ID: "A", AuthBlocked: true}}, ActiveHighestTier: map[string]struct{}{"A": {}}})
	raw, _ := json.Marshal(affinityTestRequest(t.Name(), ""))
	encoded, err := handleSchedulerPick(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(encoded, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil || env.Error.HTTPStatus != 503 {
		t.Fatalf("unavailable fell through: %s", encoded)
	}
	registration, _ := json.Marshal(PluginRegistration())
	if !PluginRegistration().Capabilities.RequestInterceptor || strings.Contains(string(registration), "scheduler_session_affinity") {
		t.Fatal("plugin still requires custom host capability")
	}
}

func TestPluginOwnedAffinityConfigMigration(t *testing.T) {
	if cfg := NormalizeConfig(Config{}); cfg.AffinityMode != "fill-first" || cfg.AffinityTTL != 4*time.Hour {
		t.Fatalf("old config defaults: %+v", cfg)
	}
	cfg, err := DecodeConfig([]byte("affinity_mode: off\naffinity_ttl: 2h\n"))
	if err != nil || cfg.AffinityMode != "off" || cfg.AffinityTTL != 2*time.Hour {
		t.Fatalf("config: %+v %v", cfg, err)
	}
	if _, err := DecodeConfig([]byte("affinity_ttl: -1s")); err == nil {
		t.Fatal("invalid TTL accepted")
	}
}
