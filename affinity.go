package main

import (
	"container/list"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const affinityRequestHeader = "X-Cpa-Quota-Affinity"

type affinityBinding struct {
	key, authID string
	instance    AuthInstanceID
	generation  uint64
	expiresAt   time.Time
	ttl         time.Duration
	element     *list.Element
}

type affinityRequest struct {
	id, token, key, authID string
	providers              []string
	generation             uint64
	attempted              bool
	expiresAt              time.Time
	element                *list.Element
}

// Affinity is plugin-owned and deliberately in-memory. Hashes isolate callers,
// provider pools and models without retaining prompts, API keys or session IDs.
type affinityCache struct {
	mu                       sync.Mutex
	bindings                 map[string]*affinityBinding
	requests                 map[string]*affinityRequest
	requestIDs               map[string]string
	boundOrder, requestOrder *list.List
	generation               uint64
	capacity                 int
}

func newAffinityCache(capacity int) *affinityCache {
	return &affinityCache{bindings: make(map[string]*affinityBinding), requests: make(map[string]*affinityRequest), requestIDs: make(map[string]string), boundOrder: list.New(), requestOrder: list.New(), capacity: capacity}
}

var globalAffinity = newAffinityCache(16384)

func (c *affinityCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.bindings)
	clear(c.requests)
	clear(c.requestIDs)
	c.boundOrder.Init()
	c.requestOrder.Init()
}

func applyAffinityConfig(cfg *Config, mode, ttl string) error {
	if mode != "" {
		if mode != "fill-first" && mode != "off" {
			return fmt.Errorf("affinity_mode must be fill-first or off")
		}
		cfg.AffinityMode = mode
	}
	if ttl != "" {
		d, err := time.ParseDuration(ttl)
		if err != nil || d <= 0 {
			return fmt.Errorf("affinity_ttl must be a positive duration")
		}
		cfg.AffinityTTL = d
	}
	return nil
}

func affinityHeader(headers map[string][]string, key string) string {
	for name, values := range headers {
		if strings.EqualFold(name, key) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

func affinityProviders(req pluginapi.SchedulerPickRequest) []string {
	set := make(map[string]struct{})
	for _, p := range append(append([]string(nil), req.Providers...), req.Provider) {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" && p != "mixed" {
			set[p] = struct{}{}
		}
	}
	providers := make([]string, 0, len(set))
	for p := range set {
		providers = append(providers, p)
	}
	sort.Strings(providers)
	return providers
}

func affinityKey(req pluginapi.SchedulerPickRequest, providers []string) string {
	caller := metadataString(req.Options.Metadata, "caller_scope")
	// CPA main resolves header, body, prompt-cache and execution identities
	// before Scheduler.Pick. Never substitute the body-dependent derived ID.
	session := metadataString(req.Options.Metadata, "canonical_session_id")
	if session == "" {
		for _, name := range []string{"X-Claude-Code-Session-Id", "Session-Id", "Session_id", "X-Session-ID", "X-Session-Affinity", "X-Client-Request-Id"} {
			if id := affinityHeader(req.Options.Headers, name); id != "" {
				session = strings.ToLower(name) + ":" + id
				break
			}
		}
	}
	if session == "" {
		if id := metadataString(req.Options.Metadata, "execution_session_id"); id != "" {
			session = "execution:" + id
		}
	}
	if session == "" && caller == "" {
		return ""
	}
	model := strings.TrimSpace(req.Model)
	if i := strings.LastIndexByte(model, '('); i > 0 && strings.HasSuffix(model, ")") {
		model = strings.TrimSpace(model[:i])
	}
	raw, _ := json.Marshal(struct {
		Caller         string
		Providers      []string
		Model, Session string
	}{caller, providers, model, session})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func (c *affinityCache) forgetRequest(r *affinityRequest) {
	delete(c.requests, r.token)
	delete(c.requestIDs, r.id)
	c.requestOrder.Remove(r.element)
}

func (c *affinityCache) forgetBinding(b *affinityBinding) {
	delete(c.bindings, b.key)
	c.boundOrder.Remove(b.element)
}

func (c *affinityCache) before(req pluginapi.RequestInterceptRequest, now time.Time) string {
	if req.RequestID == "" {
		return ""
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ""
	}
	token := hex.EncodeToString(nonce[:])
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.requests[c.requestIDs[req.RequestID]]; old != nil {
		c.forgetRequest(old)
	}
	for front := c.requestOrder.Front(); front != nil; front = c.requestOrder.Front() {
		r := front.Value.(*affinityRequest)
		if len(c.requests) < c.capacity && now.Before(r.expiresAt) {
			break
		}
		c.forgetRequest(r)
	}
	r := &affinityRequest{id: req.RequestID, token: token, expiresAt: now.Add(24 * time.Hour)}
	r.element = c.requestOrder.PushBack(r)
	c.requests[token], c.requestIDs[req.RequestID] = r, token
	return token
}

func (c *affinityCache) pick(snapshot SchedulerSnapshot, req pluginapi.SchedulerPickRequest, now time.Time) PickDecision {
	if snapshot.AffinityMode == "off" {
		decision, _ := pickSnapshotAccount(snapshot, req, now, "")
		return decision
	}
	providers := affinityProviders(req)
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.requests[affinityHeader(req.Options.Headers, affinityRequestHeader)]
	if r != nil && !now.Before(r.expiresAt) {
		c.forgetRequest(r)
		r = nil
	}
	if r != nil {
		if r.providers == nil {
			r.providers = providers
		}
		providers = r.providers
	}
	key := affinityKey(req, providers)
	preferred := ""
	if b := c.bindings[key]; b != nil {
		if now.Before(b.expiresAt) {
			for _, a := range snapshot.Accounts {
				if a.ID == b.authID && a.Instance == b.instance {
					preferred = b.authID
					break
				}
			}
		} else {
			c.forgetBinding(b)
		}
	}
	decision, instance := pickSnapshotAccount(snapshot, req, now, preferred)
	if key == "" || decision.AuthID == "" {
		return decision
	}
	ttl := snapshot.AffinityTTL
	if ttl <= 0 {
		ttl = DefaultConfig().AffinityTTL
	}
	b := c.bindings[key]
	if b == nil {
		if len(c.bindings) >= c.capacity {
			c.forgetBinding(c.boundOrder.Front().Value.(*affinityBinding))
		}
		b = &affinityBinding{key: key}
		b.element = c.boundOrder.PushBack(b)
		c.bindings[key] = b
	}
	c.generation++
	b.authID, b.instance, b.generation, b.expiresAt, b.ttl = decision.AuthID, instance, c.generation, now.Add(ttl), ttl
	c.boundOrder.MoveToBack(b.element)
	if r != nil {
		r.key, r.authID, r.generation, r.attempted = key, b.authID, b.generation, false
	}
	return decision
}

func (c *affinityCache) after(req pluginapi.RequestInterceptRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.requests[c.requestIDs[req.RequestID]]; r != nil {
		r.attempted = r.authID != "" && r.authID == metadataString(req.Metadata, "selected_auth_id")
	}
}

func (c *affinityCache) complete(done pluginapi.RequestCompletion, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.requests[c.requestIDs[done.RequestID]]
	if r == nil {
		return
	}
	c.forgetRequest(r)
	if !r.attempted {
		return
	}
	if selected := metadataString(done.Metadata, "selected_auth_id"); selected != "" && selected != r.authID {
		return
	}
	b := c.bindings[r.key]
	if b == nil || b.authID != r.authID || b.generation != r.generation {
		return
	}
	if done.Outcome == pluginapi.RequestCompletionSucceeded {
		b.expiresAt = now.Add(b.ttl)
		c.boundOrder.MoveToBack(b.element)
	} else if done.Outcome == pluginapi.RequestCompletionFailed && credentialFailureStatus(done.StatusCode) {
		c.forgetBinding(b)
	}
}

func credentialFailureStatus(status int) bool {
	switch status {
	case 401, 429, 500, 502, 503, 504:
		return true
	}
	return false
}

func handleAffinityRequestIntercept(raw []byte, after bool) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	resp := pluginapi.RequestInterceptResponse{ClearHeaders: []string{affinityRequestHeader}}
	if after {
		globalAffinity.after(req)
		// Main merges headers twice without carrying ClearHeaders through the
		// first merge. Overwrite the value too, so the token cannot survive into
		// execution. The Codex executor does not forward this private header.
		resp.Headers = http.Header{affinityRequestHeader: []string{""}}
	} else if cfg := globalState.Config(); cfg.HandleEnabled && cfg.AffinityMode != "off" {
		if token := globalAffinity.before(req, time.Now()); token != "" {
			// This opaque correlation header exists only between the standard
			// before-auth, scheduler, and after-auth hooks. Clear it before execution.
			resp.Headers = http.Header{affinityRequestHeader: []string{token}}
		}
	}
	return okEnvelope(resp)
}
