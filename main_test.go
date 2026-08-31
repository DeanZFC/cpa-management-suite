package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPluginRegistrationExposesPublicConfiguration(t *testing.T) {
	registration := pluginRegistration()
	if registration.Metadata.Name != "CPA Management Suite" {
		t.Fatalf("plugin name = %q", registration.Metadata.Name)
	}
	if registration.Metadata.GitHubRepository != "https://github.com/DeanZFC/cpa-management-suite" {
		t.Fatalf("repository = %q", registration.Metadata.GitHubRepository)
	}
	if len(registration.Metadata.ConfigFields) != 4 {
		t.Fatalf("config fields = %d, want 4", len(registration.Metadata.ConfigFields))
	}
	for _, field := range registration.Metadata.ConfigFields {
		if field.Name == "state_file" || field.Name == "pricing_file" {
			t.Fatalf("internal path exposed as config field: %q", field.Name)
		}
	}
}

func resetTestState(t *testing.T) {
	t.Helper()
	state.mu.Lock()
	defer state.mu.Unlock()
	state.cfg = config{Enabled: true, DefaultCapacity: 1, RejectWhenFull: true, StateFile: filepath.Join(t.TempDir(), "state.json")}
	state.accounts = make(map[string]accountConfig)
	state.usage = make(map[string]usageStats)
	state.active = make(map[string]int)
	state.pending = nil
	state.requests = make(map[string]string)
	state.pricing = make(map[string]modelPrice)
	state.pricingItems = make([]pricingEntry, 0)
	state.pricingAt = time.Time{}
	state.pricingSrc = ""
	state.usageByModel = make(map[string]map[string]tokenUsage)
	state.stateLoaded = true
}

func schedulerForTest(t *testing.T, candidates ...pluginapi.SchedulerAuthCandidate) (pluginapi.SchedulerPickResponse, error) {
	t.Helper()
	raw, err := json.Marshal(pluginapi.SchedulerPickRequest{Provider: "codex", Providers: []string{"codex"}, Candidates: candidates})
	if err != nil {
		return pluginapi.SchedulerPickResponse{}, err
	}
	responseRaw, err := schedulerPick(raw)
	if err != nil {
		return pluginapi.SchedulerPickResponse{}, err
	}
	var env envelope
	if err := json.Unmarshal(responseRaw, &env); err != nil {
		return pluginapi.SchedulerPickResponse{}, err
	}
	var response pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(env.Result, &response); err != nil {
		return pluginapi.SchedulerPickResponse{}, err
	}
	return response, nil
}

func TestSchedulerRespectsPerAccountCapacity(t *testing.T) {
	resetTestState(t)
	first, err := schedulerForTest(t, pluginapi.SchedulerAuthCandidate{ID: "a", Provider: "codex"}, pluginapi.SchedulerAuthCandidate{ID: "b", Provider: "codex"})
	if err != nil || first.AuthID != "a" {
		t.Fatalf("first pick = %#v, err=%v", first, err)
	}
	second, err := schedulerForTest(t, pluginapi.SchedulerAuthCandidate{ID: "a", Provider: "codex"}, pluginapi.SchedulerAuthCandidate{ID: "b", Provider: "codex"})
	if err != nil || second.AuthID != "b" {
		t.Fatalf("second pick = %#v, err=%v", second, err)
	}
}

func TestDisabledAccountIsSkipped(t *testing.T) {
	resetTestState(t)
	state.mu.Lock()
	state.accounts["a"] = accountConfig{Capacity: 2, Enabled: false}
	state.mu.Unlock()
	response, err := schedulerForTest(t, pluginapi.SchedulerAuthCandidate{ID: "a", Provider: "codex"}, pluginapi.SchedulerAuthCandidate{ID: "b", Provider: "codex"})
	if err != nil || response.AuthID != "b" {
		t.Fatalf("pick = %#v, err=%v", response, err)
	}
}

func TestCompletionReleasesExactlyOnce(t *testing.T) {
	resetTestState(t)
	response, err := schedulerForTest(t, pluginapi.SchedulerAuthCandidate{ID: "a", Provider: "codex"})
	if err != nil || response.AuthID != "a" {
		t.Fatalf("pick = %#v, err=%v", response, err)
	}
	afterRaw, err := json.Marshal(pluginapi.RequestInterceptRequest{RequestID: "req-1", Metadata: map[string]any{"selected_auth_id": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := requestIntercept("request.intercept_after", afterRaw); err != nil {
		t.Fatal(err)
	}
	completionRaw, err := json.Marshal(pluginapi.RequestCompletion{RequestID: "req-1", Metadata: map[string]any{"selected_auth_id": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := requestComplete(completionRaw); err != nil {
		t.Fatal(err)
	}
	if _, err := requestComplete(completionRaw); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	active := state.active["a"]
	state.mu.Unlock()
	if active != 0 {
		t.Fatalf("active = %d, want 0", active)
	}
}

func TestUsageAccumulatesAndPersistsAtomically(t *testing.T) {
	resetTestState(t)
	recordRaw, err := json.Marshal(pluginapi.UsageRecord{AuthID: "a", Failed: false, Detail: pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := usageHandle(recordRaw); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	got := state.usage["a"]
	path := state.cfg.StateFile
	state.mu.Unlock()
	if got.Requests != 1 || got.TotalTokens != 15 || got.Success != 1 {
		t.Fatalf("usage = %#v", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary state file remains: %v", err)
	}
}

func TestKeeperPricingChargesUncachedAndCacheSegments(t *testing.T) {
	resetTestState(t)
	state.mu.Lock()
	state.pricing = map[string]modelPrice{
		"claude-4": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	}
	state.mu.Unlock()

	recordRaw, err := json.Marshal(pluginapi.UsageRecord{
		AuthID: "a", Model: "anthropic/claude-4", Alias: "alias",
		Detail: pluginapi.UsageDetail{InputTokens: 1_000_000, OutputTokens: 500_000, CacheReadTokens: 200_000, CacheCreationTokens: 100_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := usageHandle(recordRaw); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	got := state.usage["a"].CostUSD
	state.mu.Unlock()
	want := 0.7*3 + 0.2*0.3 + 0.1*3.75 + 0.5*15
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost = %.12f, want %.12f", got, want)
	}
}

func TestPricingMultiplierChangesCost(t *testing.T) {
	resetTestState(t)
	state.mu.Lock()
	state.pricing = map[string]modelPrice{"gpt-5": {Input: 2, Output: 4, Multiplier: 1.5}}
	state.mu.Unlock()

	recordRaw, err := json.Marshal(pluginapi.UsageRecord{AuthID: "a", Model: "gpt-5", Detail: pluginapi.UsageDetail{InputTokens: 1_000_000, OutputTokens: 500_000}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := usageHandle(recordRaw); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	got := state.usage["a"].CostUSD
	state.mu.Unlock()
	if want := 1.5 * (2 + 0.5*4); math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost = %.12f, want %.12f", got, want)
	}
}

func TestPricingManagementSavesManualPrice(t *testing.T) {
	resetTestState(t)
	path := filepath.Join(t.TempDir(), "pricing.json")
	state.mu.Lock()
	state.cfg.PricingFile = path
	state.pricingSrc = "https://models.dev/api.json"
	state.mu.Unlock()

	body, err := json.Marshal(pricingUpdateRequest{Model: "custom-model", PromptPricePer1M: 1.25, CompletionPricePer1M: 3.5, CacheReadPricePer1M: 0.2, CacheWritePricePer1M: 2, PriceMultiplier: 1.8})
	if err != nil {
		t.Fatal(err)
	}
	response, err := managementPricing(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPut, Body: body})
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("save response = %#v, err=%v", response, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot pricingSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Models["custom-model"]; got.Multiplier != 1.8 || got.Input != 1.25 {
		t.Fatalf("saved price = %#v", got)
	}
}

func TestPricingManagementRejectsNegativeMultiplier(t *testing.T) {
	resetTestState(t)
	body, err := json.Marshal(pricingUpdateRequest{Model: "bad", PriceMultiplier: -1})
	if err != nil {
		t.Fatal(err)
	}
	response, err := managementPricing(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPut, Body: body})
	if err != nil || response.StatusCode != http.StatusBadRequest {
		t.Fatalf("response = %#v, err=%v", response, err)
	}
}

func TestPricingRefreshForceBypassesInterval(t *testing.T) {
	resetTestState(t)
	updated := time.Now().UTC()
	if pricingRefreshDue(updated, 24, false) {
		t.Fatal("regular refresh should wait within the refresh interval")
	}
	if !pricingRefreshDue(updated, 24, true) {
		t.Fatal("forced refresh should bypass the refresh interval")
	}
	if !pricingRefreshDue(time.Time{}, 24, false) {
		t.Fatal("missing update time should trigger a refresh")
	}
}

func TestCompilePricingCatalogPrefersOfficialProviderAndSupportsPrefix(t *testing.T) {
	input := 3.0
	output := 15.0
	zero := 0.0
	catalog := map[string]pricingProvider{
		"coding-plan": {ID: "gpt-coding-plan", Models: map[string]pricingModel{
			"gpt-5": {ID: "gpt-5", Cost: pricingCost{Input: &zero, Output: &zero}},
		}},
		"openai": {ID: "openai", Models: map[string]pricingModel{
			"gpt-5": {ID: "gpt-5", Cost: pricingCost{Input: &input, Output: &output}},
		}},
	}
	prices := compilePricingCatalog(catalog)
	price, ok := lookupPrice(prices, "openai/gpt-5")
	if !ok || price.Input != input || price.Output != output {
		t.Fatalf("price = %#v, ok=%v", price, ok)
	}
}

func TestMissingKeeperPricingHasZeroCost(t *testing.T) {
	resetTestState(t)
	recordRaw, err := json.Marshal(pluginapi.UsageRecord{AuthID: "a", Model: "unknown", Detail: pluginapi.UsageDetail{InputTokens: 100}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := usageHandle(recordRaw); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	got := state.usage["a"].CostUSD
	state.mu.Unlock()
	if got != 0 {
		t.Fatalf("cost = %v, want 0", got)
	}
}

func TestFullPoolReturns429WhenConfigured(t *testing.T) {
	resetTestState(t)
	if _, err := schedulerForTest(t, pluginapi.SchedulerAuthCandidate{ID: "a", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(pluginapi.SchedulerPickRequest{Provider: "codex", Providers: []string{"codex"}, Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "codex"}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := schedulerPick(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(result, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil || env.Error.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("envelope = %#v", env)
	}
}

func TestAuthMetadataEditingPreservesCredentialsAndUpdatesCPAFields(t *testing.T) {
	metadata, err := authMetadata(json.RawMessage(`{"type":"codex","email":"user@example.com","refresh_token":"secret"}`))
	if err != nil {
		t.Fatal(err)
	}
	priority := 7
	websockets := true
	usingAPI := false
	applyAuthMetadata(metadata, authAccountRequest{
		ProxyURL:   "http://127.0.0.1:9000",
		BaseURL:    "https://api.example.com",
		Note:       "production",
		Priority:   &priority,
		Websockets: &websockets,
		UsingAPI:   &usingAPI,
	}, true)
	if metadata["refresh_token"] != "secret" || metadata["proxy-url"] != "http://127.0.0.1:9000" || metadata["base-url"] != "https://api.example.com" {
		t.Fatalf("metadata lost credential or fields: %#v", metadata)
	}
	if metadata["priority"] != priority || metadata["websockets"] != websockets || metadata["using-api"] != usingAPI {
		t.Fatalf("metadata fields = %#v", metadata)
	}
}

func TestGeneratedAuthFileNameIsSafeAndJSON(t *testing.T) {
	name := generatedAuthFileName(map[string]any{"type": "codex", "email": "user@example.com"})
	if !strings.HasPrefix(name, "codex-userexample.com-") || !strings.HasSuffix(name, ".json") {
		t.Fatalf("generated name = %q", name)
	}
	if strings.ContainsAny(name, "/\\") {
		t.Fatalf("generated name contains path separator: %q", name)
	}
}
