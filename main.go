package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);
typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;
static void store_host_api(const cliproxy_host_api* host) { stored_host = host; }
static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) { return 1; }
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}
static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginID        = "cpa-management-suite"
	pluginVersion   = "0.4.0"
	dashboardPath   = "/dashboard"
	accountsPath    = "/account-capacity/accounts"
	authAccountPath = "/account-capacity/accounts/auth"
	pricingPath     = "/account-capacity/pricing"
	pricingSyncPath = "/account-capacity/pricing/sync"
	resetUsagePath  = "/account-capacity/reset-usage"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type config struct {
	Enabled             bool   `yaml:"enabled"`
	CPAManagementKey    string `yaml:"cpa_management_key"`
	StateFile           string `yaml:"state_file"`
	DefaultCapacity     int    `yaml:"default_capacity"`
	RejectWhenFull      bool   `yaml:"reject_when_full"`
	PricingFile         string `yaml:"pricing_file"`
	PricingURL          string `yaml:"pricing_url"`
	PricingRefreshHours int    `yaml:"pricing_refresh_hours"`
}

type modelPrice struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Multiplier float64 `json:"multiplier,omitempty"`
}

type pricingProvider struct {
	ID     string                  `json:"id"`
	Name   string                  `json:"name"`
	Models map[string]pricingModel `json:"models"`
}

type pricingModel struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Family      string      `json:"family"`
	LastUpdated string      `json:"last_updated"`
	Status      string      `json:"status"`
	Cost        pricingCost `json:"cost"`
}

type pricingCost struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

type pricingSnapshot struct {
	UpdatedAt time.Time             `json:"updated_at"`
	Source    string                `json:"source"`
	Models    map[string]modelPrice `json:"models"`
	Entries   []pricingEntry        `json:"entries,omitempty"`
}

type pricingEntry struct {
	Model        string     `json:"model"`
	Provider     string     `json:"provider,omitempty"`
	Price        modelPrice `json:"price"`
	PricingStyle string     `json:"pricing_style,omitempty"`
}

type pricingView struct {
	Model                string  `json:"model"`
	Provider             string  `json:"provider,omitempty"`
	PricingStyle         string  `json:"pricing_style"`
	PromptPricePer1M     float64 `json:"prompt_price_per_1m"`
	CompletionPricePer1M float64 `json:"completion_price_per_1m"`
	CacheReadPricePer1M  float64 `json:"cache_read_price_per_1m"`
	CacheWritePricePer1M float64 `json:"cache_write_price_per_1m"`
	PriceMultiplier      float64 `json:"price_multiplier"`
}

type pricingListResponse struct {
	Pricing    []pricingView `json:"pricing"`
	Source     string        `json:"source"`
	UpdatedAt  time.Time     `json:"updated_at,omitempty"`
	ModelCount int           `json:"model_count"`
}

type pricingUpdateRequest struct {
	Model                string  `json:"model"`
	PromptPricePer1M     float64 `json:"prompt_price_per_1m"`
	CompletionPricePer1M float64 `json:"completion_price_per_1m"`
	CacheReadPricePer1M  float64 `json:"cache_read_price_per_1m"`
	CacheWritePricePer1M float64 `json:"cache_write_price_per_1m"`
	PriceMultiplier      float64 `json:"price_multiplier"`
}

type accountConfig struct {
	Capacity int    `json:"capacity"`
	Enabled  bool   `json:"enabled"`
	Label    string `json:"label,omitempty"`
}

type usageStats struct {
	Requests          int64     `json:"requests"`
	Success           int64     `json:"success"`
	Failed            int64     `json:"failed"`
	InputTokens       int64     `json:"input_tokens"`
	OutputTokens      int64     `json:"output_tokens"`
	ReasoningTokens   int64     `json:"reasoning_tokens"`
	CachedTokens      int64     `json:"cached_tokens"`
	CacheReadTokens   int64     `json:"cache_read_tokens"`
	CacheCreateTokens int64     `json:"cache_creation_tokens"`
	TotalTokens       int64     `json:"total_tokens"`
	CostUSD           float64   `json:"cost_usd"`
	LastRequestAt     time.Time `json:"last_request_at,omitempty"`
}

type tokenUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	CacheReadTokens   int64 `json:"cache_read_tokens"`
	CacheCreateTokens int64 `json:"cache_creation_tokens"`
}

type persistedState struct {
	Accounts     map[string]accountConfig         `json:"accounts"`
	Usage        map[string]usageStats            `json:"usage"`
	UsageByModel map[string]map[string]tokenUsage `json:"usage_by_model,omitempty"`
}

type reservation struct {
	AuthID    string
	CreatedAt time.Time
}

type runtimeState struct {
	mu           sync.Mutex
	cfg          config
	accounts     map[string]accountConfig
	usage        map[string]usageStats
	active       map[string]int
	pending      []reservation
	requests     map[string]string
	pricing      map[string]modelPrice
	pricingItems []pricingEntry
	pricingAt    time.Time
	pricingSrc   string
	usageByModel map[string]map[string]tokenUsage
	stateLoaded  bool
}

var state = &runtimeState{
	cfg:          defaultConfig(),
	accounts:     make(map[string]accountConfig),
	usage:        make(map[string]usageStats),
	active:       make(map[string]int),
	requests:     make(map[string]string),
	pricing:      make(map[string]modelPrice),
	pricingItems: make([]pricingEntry, 0),
	usageByModel: make(map[string]map[string]tokenUsage),
}

const defaultPricingURL = "https://models.dev/api.json"

var pricingLoop struct {
	sync.Mutex
	stop    chan struct{}
	running bool
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  registrationCaps   `json:"capabilities"`
}

type registrationCaps struct {
	Scheduler          bool `json:"scheduler"`
	RequestInterceptor bool `json:"request_interceptor"`
	RequestLifecycle   bool `json:"request_lifecycle_plugin"`
	Usage              bool `json:"usage_plugin"`
	Management         bool `json:"management_api"`
}

type managementRequest struct {
	Method  string      `json:"Method"`
	Path    string      `json:"Path"`
	Headers http.Header `json:"Headers"`
	Query   url.Values  `json:"Query"`
	Body    []byte      `json:"Body"`
}

type accountUpdate struct {
	ID       string `json:"id"`
	Capacity int    `json:"capacity"`
	Enabled  *bool  `json:"enabled"`
	Label    string `json:"label"`
}

type authAccountRequest struct {
	AuthIndex  string            `json:"auth_index"`
	Name       string            `json:"name"`
	JSON       json.RawMessage   `json:"json"`
	Capacity   int               `json:"capacity"`
	Enabled    *bool             `json:"enabled"`
	Label      string            `json:"label"`
	ProxyURL   string            `json:"proxy_url"`
	BaseURL    string            `json:"base_url"`
	Note       string            `json:"note"`
	Priority   *int              `json:"priority"`
	Websockets *bool             `json:"websockets"`
	UsingAPI   *bool             `json:"using_api"`
	Headers    map[string]string `json:"headers"`
}

type managementRegistrationResponse struct {
	Routes []managementRoute `json:"routes,omitempty"`
}

type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type accountView struct {
	ID            string     `json:"id"`
	AuthIndex     string     `json:"auth_index,omitempty"`
	Name          string     `json:"name"`
	Label         string     `json:"label,omitempty"`
	Email         string     `json:"email,omitempty"`
	Provider      string     `json:"provider"`
	Type          string     `json:"type,omitempty"`
	Status        string     `json:"status,omitempty"`
	StatusMessage string     `json:"status_message,omitempty"`
	Disabled      bool       `json:"disabled"`
	Unavailable   bool       `json:"unavailable"`
	Capacity      int        `json:"capacity"`
	Enabled       bool       `json:"enabled"`
	RuntimeOnly   bool       `json:"runtime_only"`
	Source        string     `json:"source,omitempty"`
	Path          string     `json:"path,omitempty"`
	ProjectID     string     `json:"project_id,omitempty"`
	AccountType   string     `json:"account_type,omitempty"`
	Account       string     `json:"account,omitempty"`
	Priority      int        `json:"priority,omitempty"`
	Note          string     `json:"note,omitempty"`
	Websockets    bool       `json:"websockets"`
	UpdatedAt     time.Time  `json:"updated_at,omitempty"`
	Active        int        `json:"active"`
	Usage         usageStats `json:"usage"`
}

type accountsResponse struct {
	Accounts []accountView `json:"accounts"`
	Totals   totalsView    `json:"totals"`
}

type totalsView struct {
	Accounts    int     `json:"accounts"`
	Enabled     int     `json:"enabled"`
	Active      int     `json:"active"`
	Capacity    int     `json:"capacity"`
	Requests    int64   `json:"requests"`
	Success     int64   `json:"success"`
	Failed      int64   `json:"failed"`
	TotalTokens int64   `json:"total_tokens"`
	CostUSD     float64 `json:"cost_usd"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var raw []byte
	if request != nil && requestLen > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	result, err := handleMethod(C.GoString(method), raw)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, result)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	pricingLoop.Lock()
	if pricingLoop.stop != nil {
		close(pricingLoop.stop)
		pricingLoop.stop = nil
	}
	pricingLoop.running = false
	pricingLoop.Unlock()
	state.mu.Lock()
	state.active = make(map[string]int)
	state.pending = nil
	state.requests = make(map[string]string)
	state.mu.Unlock()
}

func handleMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(raw); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodSchedulerPick:
		return schedulerPick(raw)
	case pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter:
		return requestIntercept(method, raw)
	case pluginabi.MethodRequestComplete:
		return requestComplete(raw)
	case pluginabi.MethodUsageHandle:
		return usageHandle(raw)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return managementHandle(raw)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func defaultConfig() config {
	return config{
		Enabled:             true,
		StateFile:           "/CLIProxyAPI/plugins/account-capacity-state.json",
		DefaultCapacity:     1,
		PricingFile:         "/CLIProxyAPI/plugins/account-capacity-pricing.json",
		PricingURL:          defaultPricingURL,
		PricingRefreshHours: 24,
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return fmt.Errorf("decode lifecycle request: %w", err)
		}
	}
	cfg := defaultConfig()
	if len(req.ConfigYAML) > 0 {
		if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
			return fmt.Errorf("decode plugin config: %w", err)
		}
	}
	if cfg.DefaultCapacity < 1 {
		cfg.DefaultCapacity = 1
	}
	if cfg.StateFile == "" {
		cfg.StateFile = defaultConfig().StateFile
	}
	if cfg.PricingFile == "" {
		cfg.PricingFile = defaultConfig().PricingFile
	}
	if cfg.PricingURL == "" {
		cfg.PricingURL = defaultPricingURL
	}
	if cfg.PricingRefreshHours < 1 {
		cfg.PricingRefreshHours = 24
	}
	state.mu.Lock()
	state.cfg = cfg
	if !state.stateLoaded {
		if err := loadStateLocked(cfg.StateFile, cfg.DefaultCapacity); err != nil {
			state.mu.Unlock()
			return err
		}
		state.stateLoaded = true
	}
	loadPricingFileLocked(cfg.PricingFile)
	state.mu.Unlock()
	startPricingRefreshLoop()
	return nil
}

func startPricingRefreshLoop() {
	pricingLoop.Lock()
	if pricingLoop.running {
		pricingLoop.Unlock()
		return
	}
	stop := make(chan struct{})
	pricingLoop.stop = stop
	pricingLoop.running = true
	pricingLoop.Unlock()
	go func() {
		refreshPricing(currentConfig())
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				refreshPricing(currentConfig())
			case <-stop:
				return
			}
		}
	}()
}

func currentConfig() config {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.cfg
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name: "CPA Management Suite", Version: pluginVersion, Author: "DeanZFC",
			GitHubRepository: "https://github.com/DeanZFC/cpa-management-suite",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "cpa_management_key", Type: pluginapi.ConfigFieldTypeString, Description: "CPA Management Key；页面会使用此密钥访问 CPA 管理接口，请勿分享插件页面。"},
				{Name: "default_capacity", Type: pluginapi.ConfigFieldTypeInteger, Description: "默认每个账号的最大并发数；单个账号可在账号管理页面单独调整。"},
				{Name: "reject_when_full", Type: pluginapi.ConfigFieldTypeBoolean, Description: "所有可用账号达到容量时返回 429，而不是交给 CPA 继续选择。"},
				{Name: "pricing_url", Type: pluginapi.ConfigFieldTypeString, Description: "模型价格目录地址；留空使用 Models.dev。"},
				{Name: "pricing_refresh_hours", Type: pluginapi.ConfigFieldTypeInteger, Description: "价格目录刷新间隔（小时）；留空使用 24 小时。"},
			},
		},
		Capabilities: registrationCaps{Scheduler: true, RequestInterceptor: true, RequestLifecycle: true, Usage: true, Management: true},
	}
}

func schedulerPick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode scheduler request: %w", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.cfg.Enabled {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	best := pluginapi.SchedulerAuthCandidate{}
	bestActive, bestCapacity, bestPriority := int(^uint(0)>>1), 0, int(^uint(0)>>1)
	found := false
	for _, candidate := range req.Candidates {
		if !candidateEligible(candidate, req) {
			continue
		}
		cfg := state.accountConfigLocked(candidate.ID)
		capacity := cfg.Capacity
		active := state.active[candidate.ID]
		if active >= capacity {
			continue
		}
		if !found || active*bestCapacity < bestActive*capacity || (active*bestCapacity == bestActive*capacity && (active < bestActive || (active == bestActive && candidate.Priority < bestPriority))) {
			best, bestActive, bestCapacity, bestPriority, found = candidate, active, capacity, candidate.Priority, true
		}
	}
	if !found {
		if state.cfg.RejectWhenFull {
			return schedulerError(http.StatusTooManyRequests, "all eligible accounts are at capacity")
		}
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	state.active[best.ID]++
	state.pending = append(state.pending, reservation{AuthID: best.ID, CreatedAt: time.Now()})
	return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: best.ID, Handled: true})
}

func candidateEligible(candidate pluginapi.SchedulerAuthCandidate, req pluginapi.SchedulerPickRequest) bool {
	if strings.TrimSpace(candidate.ID) == "" || strings.EqualFold(candidate.Status, "disabled") || strings.EqualFold(candidate.Status, "error") {
		return false
	}
	if !state.accountConfigLocked(candidate.ID).Enabled {
		return false
	}
	if len(req.Providers) > 0 {
		match := false
		for _, provider := range req.Providers {
			if strings.EqualFold(strings.TrimSpace(provider), strings.TrimSpace(candidate.Provider)) {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	return true
}

func requestIntercept(method string, raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode request intercept: %w", err)
	}
	if method == pluginabi.MethodRequestInterceptBefore {
		return okEnvelope(pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: req.Body})
	}
	selectedID := metadataString(req.Metadata, "selected_auth_id")
	state.mu.Lock()
	defer state.mu.Unlock()
	if selectedID != "" {
		state.bindReservationLocked(req.RequestID, selectedID)
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: req.Body})
}

func requestComplete(raw []byte) ([]byte, error) {
	var completion pluginapi.RequestCompletion
	if err := json.Unmarshal(raw, &completion); err != nil {
		return nil, fmt.Errorf("decode request completion: %w", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if authID, ok := state.requests[completion.RequestID]; ok {
		state.releaseLocked(authID)
		delete(state.requests, completion.RequestID)
	} else if selectedID := metadataString(completion.Metadata, "selected_auth_id"); selectedID != "" {
		state.releasePendingLocked(selectedID)
	}
	return okEnvelope(struct{}{})
}

func usageHandle(raw []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, fmt.Errorf("decode usage record: %w", err)
	}
	authID := strings.TrimSpace(record.AuthID)
	if authID == "" {
		return okEnvelope(struct{}{})
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.usage == nil {
		state.usage = make(map[string]usageStats)
	}
	item := state.usage[authID]
	item.Requests++
	if record.Failed {
		item.Failed++
	} else {
		item.Success++
	}
	item.InputTokens += record.Detail.InputTokens
	item.OutputTokens += record.Detail.OutputTokens
	item.ReasoningTokens += record.Detail.ReasoningTokens
	item.CachedTokens += record.Detail.CachedTokens
	item.CacheReadTokens += record.Detail.CacheReadTokens
	item.CacheCreateTokens += record.Detail.CacheCreationTokens
	item.TotalTokens += record.Detail.TotalTokens
	item.CostUSD += estimateCostLocked(record)
	modelKey := strings.TrimSpace(record.Model)
	if modelKey == "" {
		modelKey = strings.TrimSpace(record.Alias)
	}
	if modelKey != "" {
		if state.usageByModel == nil {
			state.usageByModel = make(map[string]map[string]tokenUsage)
		}
		if state.usageByModel[authID] == nil {
			state.usageByModel[authID] = make(map[string]tokenUsage)
		}
		modelTotals := state.usageByModel[authID][modelKey]
		modelTotals.InputTokens += record.Detail.InputTokens
		modelTotals.OutputTokens += record.Detail.OutputTokens
		modelTotals.CacheReadTokens += record.Detail.CacheReadTokens
		modelTotals.CacheCreateTokens += record.Detail.CacheCreationTokens
		state.usageByModel[authID][modelKey] = modelTotals
	}
	if record.RequestedAt.IsZero() {
		item.LastRequestAt = time.Now()
	} else {
		item.LastRequestAt = record.RequestedAt
	}
	state.usage[authID] = item
	if err := saveStateLocked(); err != nil {
		return nil, err
	}
	return okEnvelope(struct{}{})
}

func estimateCostLocked(record pluginapi.UsageRecord) float64 {
	price, ok := findPriceLocked(record.Model, record.Alias)
	if !ok {
		return 0
	}
	input := maxInt64(record.Detail.InputTokens, 0)
	output := maxInt64(record.Detail.OutputTokens, 0)
	cacheRead := maxInt64(record.Detail.CacheReadTokens, 0)
	cacheWrite := maxInt64(record.Detail.CacheCreationTokens, 0)
	normalInput := input - cacheRead - cacheWrite
	if normalInput < 0 {
		normalInput = 0
	}
	multiplier := price.Multiplier
	if multiplier == 0 {
		multiplier = 1
	}
	return multiplier * ((float64(normalInput)/1_000_000)*price.Input +
		(float64(cacheRead)/1_000_000)*price.CacheRead +
		(float64(cacheWrite)/1_000_000)*price.CacheWrite +
		(float64(output)/1_000_000)*price.Output)
}

func maxInt64(value, floor int64) int64 {
	if value < floor {
		return floor
	}
	return value
}

func findPriceLocked(model, alias string) (modelPrice, bool) {
	for _, candidate := range []string{model, alias} {
		if price, ok := lookupPrice(state.pricing, candidate); ok {
			return price, true
		}
	}
	return modelPrice{}, false
}

func lookupPrice(prices map[string]modelPrice, raw string) (modelPrice, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return modelPrice{}, false
	}
	for _, key := range []string{raw, stripModelPrefix(raw), normalizeModelKey(raw), normalizeModelKey(stripModelPrefix(raw))} {
		if key == "" {
			continue
		}
		if price, ok := prices[strings.ToLower(key)]; ok {
			return price, true
		}
	}
	return modelPrice{}, false
}

func loadPricingFileLocked(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var snapshot pricingSnapshot
	if json.Unmarshal(raw, &snapshot) != nil || len(snapshot.Models) == 0 {
		return
	}
	state.pricing = snapshot.Models
	state.pricingItems = snapshot.Entries
	if len(state.pricingItems) == 0 {
		state.pricingItems = pricingItemsFromMap(snapshot.Models)
	}
	state.pricingAt = snapshot.UpdatedAt
	state.pricingSrc = snapshot.Source
}

func refreshPricing(cfg config) {
	_ = refreshPricingNow(cfg, false)
}

func refreshPricingNow(cfg config, force bool) error {
	state.mu.Lock()
	if !pricingRefreshDue(state.pricingAt, cfg.PricingRefreshHours, force) {
		state.mu.Unlock()
		return nil
	}
	state.mu.Unlock()

	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Get(cfg.PricingURL)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var catalog map[string]pricingProvider
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<20))
	if err := decoder.Decode(&catalog); err != nil {
		return err
	}
	prices := compilePricingCatalog(catalog)
	if len(prices) == 0 {
		return fmt.Errorf("价格目录为空")
	}
	now := time.Now().UTC()
	items := pricingItemsFromCatalog(catalog)
	snapshot := pricingSnapshot{UpdatedAt: now, Source: cfg.PricingURL, Models: prices, Entries: items}
	if err := savePricingFile(cfg.PricingFile, snapshot); err != nil {
		return err
	}
	state.mu.Lock()
	state.pricing = prices
	state.pricingItems = items
	state.pricingAt = now
	state.pricingSrc = cfg.PricingURL
	state.mu.Unlock()
	return nil
}

func pricingRefreshDue(updatedAt time.Time, refreshHours int, force bool) bool {
	if force || updatedAt.IsZero() {
		return true
	}
	return time.Since(updatedAt) >= time.Duration(refreshHours)*time.Hour
}

func savePricingFile(path string, snapshot pricingSnapshot) error {
	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

type catalogPricingEntry struct {
	Provider string
	Model    pricingModel
	Price    modelPrice
}

func compilePricingCatalog(catalog map[string]pricingProvider) map[string]modelPrice {
	entries := make([]catalogPricingEntry, 0)
	for providerKey, provider := range catalog {
		providerID := strings.TrimSpace(provider.ID)
		if providerID == "" {
			providerID = strings.TrimSpace(providerKey)
		}
		for modelKey, item := range provider.Models {
			modelID := strings.TrimSpace(item.ID)
			if modelID == "" {
				modelID = strings.TrimSpace(modelKey)
			}
			if modelID == "" || item.Cost.Input == nil || item.Cost.Output == nil {
				continue
			}
			price := modelPrice{Input: *item.Cost.Input, Output: *item.Cost.Output}
			if item.Cost.CacheRead != nil {
				price.CacheRead = *item.Cost.CacheRead
			}
			if item.Cost.CacheWrite != nil {
				price.CacheWrite = *item.Cost.CacheWrite
			}
			if !validPrice(price) {
				continue
			}
			item.ID = modelID
			entries = append(entries, catalogPricingEntry{Provider: providerID, Model: item, Price: price})
		}
	}

	prices := make(map[string]modelPrice, len(entries)*4)
	chosen := make(map[string]catalogPricingEntry)
	for _, entry := range entries {
		for _, modelKey := range []string{entry.Model.ID, entry.Model.Name} {
			key := strings.ToLower(strings.TrimSpace(modelKey))
			if key == "" {
				continue
			}
			if old, ok := chosen[key]; !ok || betterPricingEntry(entry, old) {
				chosen[key] = entry
			}
		}
	}
	for key, entry := range chosen {
		prices[key] = entry.Price
		prices[strings.ToLower(stripModelPrefix(key))] = entry.Price
		prices[normalizeModelKey(key)] = entry.Price
		prices[normalizeModelKey(stripModelPrefix(key))] = entry.Price
	}
	return prices
}

func pricingItemsFromCatalog(catalog map[string]pricingProvider) []pricingEntry {
	chosen := make(map[string]catalogPricingEntry)
	for providerKey, provider := range catalog {
		providerID := strings.TrimSpace(provider.ID)
		if providerID == "" {
			providerID = strings.TrimSpace(providerKey)
		}
		for modelKey, item := range provider.Models {
			modelID := strings.TrimSpace(item.ID)
			if modelID == "" {
				modelID = strings.TrimSpace(modelKey)
			}
			if modelID == "" || item.Cost.Input == nil || item.Cost.Output == nil {
				continue
			}
			price := modelPrice{Input: *item.Cost.Input, Output: *item.Cost.Output}
			if item.Cost.CacheRead != nil {
				price.CacheRead = *item.Cost.CacheRead
			}
			if item.Cost.CacheWrite != nil {
				price.CacheWrite = *item.Cost.CacheWrite
			}
			if !validPrice(price) {
				continue
			}
			item.ID = modelID
			key := strings.ToLower(modelID)
			entry := catalogPricingEntry{Provider: providerID, Model: item, Price: price}
			if old, ok := chosen[key]; !ok || betterPricingEntry(entry, old) {
				chosen[key] = entry
			}
		}
	}
	result := make([]pricingEntry, 0, len(chosen))
	for _, item := range chosen {
		result = append(result, pricingEntry{Model: item.Model.ID, Provider: item.Provider, Price: item.Price, PricingStyle: pricingStyle(item.Provider)})
	}
	sort.Slice(result, func(i, j int) bool { return strings.ToLower(result[i].Model) < strings.ToLower(result[j].Model) })
	return result
}

func pricingItemsFromMap(prices map[string]modelPrice) []pricingEntry {
	result := make([]pricingEntry, 0, len(prices))
	seen := make(map[string]struct{})
	for key, price := range prices {
		key = strings.TrimSpace(key)
		if key == "" || strings.ContainsAny(key, "/:") || strings.Contains(strings.ToLower(key), " ") {
			continue
		}
		canonical := strings.ToLower(stripModelPrefix(key))
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		result = append(result, pricingEntry{Model: key, Price: price, PricingStyle: "OpenAI"})
	}
	sort.Slice(result, func(i, j int) bool { return strings.ToLower(result[i].Model) < strings.ToLower(result[j].Model) })
	return result
}

func pricingStyle(provider string) string {
	provider = strings.ToLower(provider)
	if strings.Contains(provider, "anthropic") || strings.Contains(provider, "claude") {
		return "Claude"
	}
	return "OpenAI"
}

func validPrice(price modelPrice) bool {
	return price.Input >= 0 && price.Output >= 0 && price.CacheRead >= 0 && price.CacheWrite >= 0 &&
		!math.IsNaN(price.Input) && !math.IsNaN(price.Output) && !math.IsNaN(price.CacheRead) && !math.IsNaN(price.CacheWrite) &&
		!math.IsInf(price.Input, 0) && !math.IsInf(price.Output, 0) && !math.IsInf(price.CacheRead, 0) && !math.IsInf(price.CacheWrite, 0)
}

func betterPricingEntry(left, right catalogPricingEntry) bool {
	leftPlanZero := isPlanProvider(left.Provider) && isZeroPlanPrice(left.Price)
	rightPlanZero := isPlanProvider(right.Provider) && isZeroPlanPrice(right.Price)
	if leftPlanZero != rightPlanZero {
		return !leftPlanZero
	}
	leftRank := pricingProviderRank(left.Provider, left.Model.ID)
	rightRank := pricingProviderRank(right.Provider, right.Model.ID)
	if leftRank != rightRank {
		return leftRank < rightRank
	}
	leftDeprecated := strings.EqualFold(strings.TrimSpace(left.Model.Status), "deprecated")
	rightDeprecated := strings.EqualFold(strings.TrimSpace(right.Model.Status), "deprecated")
	if leftDeprecated != rightDeprecated {
		return !leftDeprecated
	}
	if left.Model.LastUpdated != right.Model.LastUpdated {
		return left.Model.LastUpdated > right.Model.LastUpdated
	}
	return left.Provider < right.Provider
}

func isZeroPlanPrice(price modelPrice) bool { return price.Input == 0 && price.Output == 0 }
func isPlanProvider(provider string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	return strings.Contains(provider, "coding-plan") || strings.Contains(provider, "token-plan")
}

func pricingProviderRank(provider, model string) int {
	provider = strings.ToLower(strings.TrimSpace(provider))
	family := modelFamily(model)
	providers := map[string][]string{
		"openai":    {"openai", "azure", "azure-cognitive-services"},
		"anthropic": {"anthropic", "google-vertex-anthropic"},
		"deepseek":  {"deepseek", "siliconflow-cn", "siliconflow"},
		"glm":       {"zai", "zhipuai", "zai-coding-plan", "zhipuai-coding-plan"},
		"qwen":      {"alibaba-cn", "alibaba", "aliyun-bailian"},
		"google":    {"google", "google-vertex"},
		"xai":       {"xai"},
		"minimax":   {"minimax-cn", "minimax", "minimax-cn-coding-plan", "minimax-coding-plan"},
		"moonshot":  {"moonshotai-cn", "moonshotai", "kimi-for-coding"},
		"doubao":    {"doubao"}, "mistral": {"mistral"}, "llama": {"llama"},
		"xiaomi": {"xiaomi-token-plan-cn", "xiaomi", "xiaomi-token-plan-sgp", "xiaomi-token-plan-ams"},
	}
	for index, candidate := range providers[family] {
		if provider == candidate {
			return index
		}
	}
	return 100
}

func modelFamily(model string) string {
	model = normalizeModelKey(stripModelPrefix(model))
	for prefix, family := range map[string]string{
		"gpt": "openai", "chatgpt": "openai", "o1": "openai", "o3": "openai", "o4": "openai",
		"claude": "anthropic", "deepseek": "deepseek", "glm": "glm", "qwen": "qwen", "gemini": "google",
		"grok": "xai", "minimax": "minimax", "moonshot": "moonshot", "kimi": "moonshot", "doubao": "doubao",
		"mistral": "mistral", "llama": "llama", "xiaomi": "xiaomi",
	} {
		if strings.HasPrefix(model, prefix) {
			return family
		}
	}
	return ""
}

func stripModelPrefix(model string) string {
	model = strings.TrimSpace(model)
	if index := strings.LastIndexAny(model, "/:"); index >= 0 && index < len(model)-1 {
		return strings.TrimSpace(model[index+1:])
	}
	return model
}

func normalizeModelKey(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(unicode.ToLower(r))
		}
	}
	return builder.String()
}

func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{Routes: []managementRoute{
		{Method: http.MethodGet, Path: accountsPath},
		{Method: http.MethodPut, Path: accountsPath},
		{Method: http.MethodPost, Path: authAccountPath, Description: "新增 CPA 认证账号。"},
		{Method: http.MethodGet, Path: authAccountPath, Description: "读取账号认证内容用于编辑。"},
		{Method: http.MethodPut, Path: authAccountPath, Description: "编辑 CPA 认证账号。"},
		{Method: http.MethodGet, Path: pricingPath, Description: "查看模型价格。"},
		{Method: http.MethodPut, Path: pricingPath, Description: "编辑模型价格。"},
		{Method: http.MethodPost, Path: pricingSyncPath, Description: "从 Models.dev 同步模型价格。"},
		{Method: http.MethodPost, Path: resetUsagePath},
		{Method: http.MethodGet, Path: dashboardPath, Menu: "账号管理", Description: "管理 CPA 账号容量、当前并发和用量统计。"},
	}}
}

func managementHandle(raw []byte) ([]byte, error) {
	var req managementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode management request: %w", err)
	}
	pluginReq := pluginapi.ManagementRequest{Method: req.Method, Path: req.Path, Headers: req.Headers, Query: req.Query, Body: req.Body}
	var resp pluginapi.ManagementResponse
	path := strings.TrimRight(req.Path, "/")
	switch {
	case strings.HasSuffix(path, authAccountPath):
		resp, _ = managementAuthAccount(context.Background(), pluginReq)
	case strings.HasSuffix(path, accountsPath):
		if req.Method == http.MethodGet {
			resp, _ = managementAccounts(context.Background(), pluginReq)
		} else if req.Method == http.MethodPut {
			resp, _ = managementUpdateAccount(context.Background(), pluginReq)
		} else {
			resp = jsonResponse(http.StatusMethodNotAllowed, nil)
		}
	case strings.HasSuffix(path, resetUsagePath):
		resp, _ = managementResetUsage(context.Background(), pluginReq)
	case strings.HasSuffix(path, pricingSyncPath):
		resp, _ = managementPricingSync(context.Background(), pluginReq)
	case strings.HasSuffix(path, pricingPath):
		resp, _ = managementPricing(context.Background(), pluginReq)
	case strings.HasSuffix(path, "/account-capacity/dashboard") || strings.HasSuffix(path, dashboardPath):
		resp, _ = managementDashboard(context.Background(), pluginReq)
	default:
		resp = jsonResponse(http.StatusNotFound, map[string]string{"error": "not found"})
	}
	return okEnvelope(resp)
}

func managementAuthAccount(_ context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	switch req.Method {
	case http.MethodGet:
		return managementGetAuthAccount(req)
	case http.MethodPost:
		return managementSaveAuthAccount(req, false)
	case http.MethodPut:
		return managementSaveAuthAccount(req, true)
	default:
		return jsonResponse(http.StatusMethodNotAllowed, nil), nil
	}
}

func managementGetAuthAccount(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	authIndex := strings.TrimSpace(req.Query.Get("auth_index"))
	if authIndex == "" {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "auth_index 必填"}), nil
	}
	result, err := callHost(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]string{"error": err.Error()}), nil
	}
	var response pluginapi.HostAuthGetResponse
	if err := json.Unmarshal(result, &response); err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]string{"error": "CPA 返回的认证内容无效"}), nil
	}
	return jsonResponse(http.StatusOK, response), nil
}

func managementSaveAuthAccount(req pluginapi.ManagementRequest, editing bool) (pluginapi.ManagementResponse, error) {
	var input authAccountRequest
	if err := json.Unmarshal(req.Body, &input); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "请求体不是有效 JSON"}), nil
	}
	input.AuthIndex = strings.TrimSpace(input.AuthIndex)
	input.Name = strings.TrimSpace(input.Name)
	var raw json.RawMessage
	if editing {
		if input.AuthIndex == "" {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "编辑账号必须提供 auth_index"}), nil
		}
		result, err := callHost(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: input.AuthIndex})
		if err != nil {
			return jsonResponse(http.StatusBadGateway, map[string]string{"error": err.Error()}), nil
		}
		var existing pluginapi.HostAuthGetResponse
		if err := json.Unmarshal(result, &existing); err != nil {
			return jsonResponse(http.StatusBadGateway, map[string]string{"error": "CPA 返回的认证内容无效"}), nil
		}
		input.Name = strings.TrimSpace(existing.Name)
		raw = existing.JSON
		if len(input.JSON) > 0 && len(strings.TrimSpace(string(input.JSON))) > 0 {
			raw = input.JSON
		}
	} else {
		raw = input.JSON
	}
	metadata, err := authMetadata(raw)
	if err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()}), nil
	}
	if input.Name == "" {
		input.Name = generatedAuthFileName(metadata)
	}
	if !strings.HasSuffix(strings.ToLower(input.Name), ".json") || strings.ContainsAny(input.Name, "/\\") {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "文件名必须是安全的 .json 文件名"}), nil
	}
	applyAuthMetadata(metadata, input, editing)
	raw, err = json.Marshal(metadata)
	if err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "认证 JSON 无法编码"}), nil
	}
	savedRaw, err := callHost(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{Name: input.Name, JSON: raw})
	if err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]string{"error": err.Error()}), nil
	}
	var saved pluginapi.HostAuthSaveResponse
	if err := json.Unmarshal(savedRaw, &saved); err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]string{"error": "CPA 保存响应无效"}), nil
	}
	entries, err := hostAuthList()
	if err != nil {
		return jsonResponse(http.StatusOK, map[string]any{"ok": true, "saved": saved.Name, "warning": err.Error()}), nil
	}
	var entry *pluginapi.HostAuthFileEntry
	for i := range entries {
		if strings.EqualFold(strings.TrimSpace(entries[i].Name), saved.Name) || entries[i].AuthIndex == input.AuthIndex {
			entry = &entries[i]
			break
		}
	}
	if entry != nil {
		capacity := input.Capacity
		if capacity < 1 {
			state.mu.Lock()
			capacity = state.accountConfigLocked(entry.ID).Capacity
			state.mu.Unlock()
		}
		if capacity < 1 {
			capacity = 1
		}
		enabled := true
		if input.Enabled != nil {
			enabled = *input.Enabled
		} else if editing {
			state.mu.Lock()
			enabled = state.accountConfigLocked(entry.ID).Enabled
			state.mu.Unlock()
		}
		state.mu.Lock()
		state.accounts[entry.ID] = accountConfig{Capacity: capacity, Enabled: enabled, Label: strings.TrimSpace(input.Label)}
		if editing && input.Label == "" {
			state.accounts[entry.ID] = accountConfig{Capacity: capacity, Enabled: enabled}
		}
		err = saveStateLocked()
		state.mu.Unlock()
		if err != nil {
			return jsonResponse(http.StatusInternalServerError, map[string]string{"error": err.Error()}), nil
		}
	}
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "saved": saved.Name, "account": entry}), nil
}

func authMetadata(raw json.RawMessage) (map[string]any, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, fmt.Errorf("认证 JSON 不能为空")
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("认证 JSON 格式错误: %w", err)
	}
	if metadata == nil {
		return nil, fmt.Errorf("认证 JSON 必须是对象")
	}
	return metadata, nil
}

func generatedAuthFileName(metadata map[string]any) string {
	provider := strings.ToLower(metadataString(metadata, "type"))
	if provider == "" {
		provider = strings.ToLower(metadataString(metadata, "provider"))
	}
	email := metadataString(metadata, "email")
	base := provider
	if base == "" {
		base = "account"
	}
	if email != "" {
		base += "-" + email
	}
	var b strings.Builder
	for _, r := range base {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	name := strings.Trim(b.String(), "-_.")
	if name == "" {
		name = "account"
	}
	return fmt.Sprintf("%s-%d.json", name, time.Now().UnixNano())
}

func applyAuthMetadata(metadata map[string]any, input authAccountRequest, editing bool) {
	setString := func(key, value string) {
		if value == "" && editing {
			delete(metadata, key)
			return
		}
		if value != "" {
			metadata[key] = value
		}
	}
	setString("proxy-url", strings.TrimSpace(input.ProxyURL))
	setString("base-url", strings.TrimSpace(input.BaseURL))
	setString("note", strings.TrimSpace(input.Note))
	if input.Priority != nil {
		metadata["priority"] = *input.Priority
	}
	if input.Websockets != nil {
		metadata["websockets"] = *input.Websockets
	}
	if input.UsingAPI != nil {
		metadata["using-api"] = *input.UsingAPI
	}
	if input.Headers != nil {
		if len(input.Headers) == 0 && editing {
			delete(metadata, "headers")
		} else {
			metadata["headers"] = input.Headers
		}
	}
}

func managementAccounts(_ context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if req.Method != http.MethodGet {
		return jsonResponse(http.StatusMethodNotAllowed, nil), nil
	}
	entries, err := hostAuthList()
	if err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]string{"error": err.Error()}), nil
	}
	return jsonResponse(http.StatusOK, buildAccountsResponse(entries)), nil
}

func managementUpdateAccount(_ context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if req.Method != http.MethodPut {
		return jsonResponse(http.StatusMethodNotAllowed, nil), nil
	}
	var update accountUpdate
	if err := json.Unmarshal(req.Body, &update); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "请求体不是有效 JSON"}), nil
	}
	update.ID = strings.TrimSpace(update.ID)
	if update.ID == "" || update.Capacity < 1 {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "id 必填，capacity 必须大于 0"}), nil
	}
	state.mu.Lock()
	cfg := state.accountConfigLocked(update.ID)
	cfg.Capacity = update.Capacity
	if update.Enabled != nil {
		cfg.Enabled = *update.Enabled
	}
	if update.Label != "" {
		cfg.Label = strings.TrimSpace(update.Label)
	}
	state.accounts[update.ID] = cfg
	err := saveStateLocked()
	state.mu.Unlock()
	if err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]string{"error": err.Error()}), nil
	}
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "account": update.ID, "capacity": cfg.Capacity, "enabled": cfg.Enabled}), nil
}

func managementResetUsage(_ context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if req.Method != http.MethodPost {
		return jsonResponse(http.StatusMethodNotAllowed, nil), nil
	}
	state.mu.Lock()
	state.usage = make(map[string]usageStats)
	state.usageByModel = make(map[string]map[string]tokenUsage)
	err := saveStateLocked()
	state.mu.Unlock()
	if err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]string{"error": err.Error()}), nil
	}
	return jsonResponse(http.StatusOK, map[string]bool{"ok": true}), nil
}

func managementDashboard(_ context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if req.Method != http.MethodGet {
		return htmlResponse(http.StatusMethodNotAllowed, "方法不允许"), nil
	}
	return htmlResponse(http.StatusOK, dashboardHTML()), nil
}

func managementPricing(_ context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	switch req.Method {
	case http.MethodGet:
		state.mu.Lock()
		response := pricingListResponse{Pricing: pricingViewsLocked(), Source: state.pricingSrc, UpdatedAt: state.pricingAt}
		response.ModelCount = len(response.Pricing)
		state.mu.Unlock()
		return jsonResponse(http.StatusOK, response), nil
	case http.MethodPut:
		var input pricingUpdateRequest
		if err := json.Unmarshal(req.Body, &input); err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "请求体不是有效 JSON"}), nil
		}
		input.Model = strings.TrimSpace(input.Model)
		if input.Model == "" || input.PriceMultiplier < 0 || math.IsNaN(input.PriceMultiplier) || math.IsInf(input.PriceMultiplier, 0) || !validPrice(modelPrice{Input: input.PromptPricePer1M, Output: input.CompletionPricePer1M, CacheRead: input.CacheReadPricePer1M, CacheWrite: input.CacheWritePricePer1M}) {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "模型和价格必须有效，价格不能为负数"}), nil
		}
		if input.PriceMultiplier == 0 {
			input.PriceMultiplier = 1
		}
		state.mu.Lock()
		price := modelPrice{Input: input.PromptPricePer1M, Output: input.CompletionPricePer1M, CacheRead: input.CacheReadPricePer1M, CacheWrite: input.CacheWritePricePer1M, Multiplier: input.PriceMultiplier}
		putPriceAliasesLocked(input.Model, price)
		upsertPricingItemLocked(pricingEntry{Model: input.Model, Price: price, PricingStyle: "OpenAI"})
		snapshot := pricingSnapshot{UpdatedAt: state.pricingAt, Source: state.pricingSrc, Models: state.pricing, Entries: state.pricingItems}
		if snapshot.UpdatedAt.IsZero() {
			snapshot.UpdatedAt = time.Now().UTC()
		}
		err := savePricingFile(state.cfg.PricingFile, snapshot)
		response := pricingListResponse{Pricing: pricingViewsLocked(), Source: state.pricingSrc, UpdatedAt: snapshot.UpdatedAt}
		response.ModelCount = len(response.Pricing)
		state.mu.Unlock()
		if err != nil {
			return jsonResponse(http.StatusInternalServerError, map[string]string{"error": err.Error()}), nil
		}
		return jsonResponse(http.StatusOK, response), nil
	default:
		return jsonResponse(http.StatusMethodNotAllowed, nil), nil
	}
}

func managementPricingSync(_ context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if req.Method != http.MethodPost {
		return jsonResponse(http.StatusMethodNotAllowed, nil), nil
	}
	if err := refreshPricingNow(currentConfig(), true); err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]string{"error": "同步 Models.dev 价格失败: " + err.Error()}), nil
	}
	state.mu.Lock()
	response := pricingListResponse{Pricing: pricingViewsLocked(), Source: state.pricingSrc, UpdatedAt: state.pricingAt}
	response.ModelCount = len(response.Pricing)
	state.mu.Unlock()
	return jsonResponse(http.StatusOK, response), nil
}

func pricingViewsLocked() []pricingView {
	items := state.pricingItems
	if len(items) == 0 {
		items = pricingItemsFromMap(state.pricing)
	}
	result := make([]pricingView, 0, len(items))
	for _, item := range items {
		multiplier := item.Price.Multiplier
		if multiplier == 0 {
			multiplier = 1
		}
		result = append(result, pricingView{Model: item.Model, Provider: item.Provider, PricingStyle: item.PricingStyle, PromptPricePer1M: item.Price.Input, CompletionPricePer1M: item.Price.Output, CacheReadPricePer1M: item.Price.CacheRead, CacheWritePricePer1M: item.Price.CacheWrite, PriceMultiplier: multiplier})
	}
	sort.Slice(result, func(i, j int) bool { return strings.ToLower(result[i].Model) < strings.ToLower(result[j].Model) })
	return result
}

func upsertPricingItemLocked(item pricingEntry) {
	for i := range state.pricingItems {
		if strings.EqualFold(state.pricingItems[i].Model, item.Model) {
			state.pricingItems[i] = item
			return
		}
	}
	state.pricingItems = append(state.pricingItems, item)
}

func putPriceAliasesLocked(model string, price modelPrice) {
	for _, key := range []string{model, stripModelPrefix(model), normalizeModelKey(model), normalizeModelKey(stripModelPrefix(model))} {
		if key != "" {
			state.pricing[strings.ToLower(key)] = price
		}
	}
}

func buildAccountsResponse(entries []pluginapi.HostAuthFileEntry) accountsResponse {
	state.mu.Lock()
	defer state.mu.Unlock()
	result := accountsResponse{Accounts: make([]accountView, 0, len(entries))}
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			id = strings.TrimSpace(entry.Name)
		}
		if id == "" {
			continue
		}
		cfg := state.accountConfigLocked(id)
		usage := state.usage[id]
		if dynamicCost, ok := state.accountCostLocked(id); ok {
			usage.CostUSD = dynamicCost
		}
		item := accountView{ID: id, AuthIndex: entry.AuthIndex, Name: entry.Name, Label: cfg.Label, Email: entry.Email, Provider: entry.Provider, Type: entry.Type, Status: entry.Status, StatusMessage: entry.StatusMessage, Disabled: entry.Disabled, Unavailable: entry.Unavailable, Capacity: cfg.Capacity, Enabled: cfg.Enabled, RuntimeOnly: entry.RuntimeOnly, Source: entry.Source, Path: entry.Path, ProjectID: entry.ProjectID, AccountType: entry.AccountType, Account: entry.Account, Priority: entry.Priority, Note: entry.Note, Websockets: entry.Websockets, UpdatedAt: entry.UpdatedAt, Active: state.active[id], Usage: usage}
		if item.Label == "" {
			item.Label = entry.Label
		}
		result.Accounts = append(result.Accounts, item)
		result.Totals.Accounts++
		if cfg.Enabled && !entry.Disabled {
			result.Totals.Enabled++
		}
		result.Totals.Active += item.Active
		result.Totals.Capacity += item.Capacity
		result.Totals.Requests += item.Usage.Requests
		result.Totals.Success += item.Usage.Success
		result.Totals.Failed += item.Usage.Failed
		result.Totals.TotalTokens += item.Usage.TotalTokens
		result.Totals.CostUSD += item.Usage.CostUSD
	}
	sort.Slice(result.Accounts, func(i, j int) bool {
		return strings.ToLower(result.Accounts[i].Name) < strings.ToLower(result.Accounts[j].Name)
	})
	return result
}

func hostAuthList() ([]pluginapi.HostAuthFileEntry, error) {
	result, err := callHost(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var response struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return nil, fmt.Errorf("decode host auth list: %w", err)
	}
	return response.Files, nil
}

func callHost(method string, payload any) (json.RawMessage, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var requestPtr *C.uint8_t
	if len(raw) > 0 {
		ptr := C.CBytes(raw)
		if ptr == nil {
			return nil, fmt.Errorf("allocate host request")
		}
		defer C.free(ptr)
		requestPtr = (*C.uint8_t)(ptr)
	}
	var response C.cliproxy_buffer
	code := C.call_host_api(cMethod, requestPtr, C.size_t(len(raw)), &response)
	var responseRaw []byte
	if response.ptr != nil && response.len > 0 {
		responseRaw = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(responseRaw) == 0 {
		return nil, fmt.Errorf("host callback returned no response, code=%d", int(code))
	}
	var env envelope
	if err := json.Unmarshal(responseRaw, &env); err != nil {
		return nil, err
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback failed")
	}
	return env.Result, nil
}

func (s *runtimeState) accountConfigLocked(id string) accountConfig {
	cfg, ok := s.accounts[id]
	if !ok {
		cfg = accountConfig{Capacity: s.cfg.DefaultCapacity, Enabled: true}
		s.accounts[id] = cfg
	}
	if cfg.Capacity < 1 {
		cfg.Capacity = s.cfg.DefaultCapacity
		if cfg.Capacity < 1 {
			cfg.Capacity = 1
		}
	}
	return cfg
}

func (s *runtimeState) accountCostLocked(authID string) (float64, bool) {
	models := s.usageByModel[authID]
	if len(models) == 0 {
		return 0, false
	}
	var total float64
	for model, usage := range models {
		price, ok := lookupPrice(s.pricing, model)
		if !ok {
			continue
		}
		input := maxInt64(usage.InputTokens, 0)
		cacheRead := maxInt64(usage.CacheReadTokens, 0)
		cacheWrite := maxInt64(usage.CacheCreateTokens, 0)
		normalInput := input - cacheRead - cacheWrite
		if normalInput < 0 {
			normalInput = 0
		}
		multiplier := price.Multiplier
		if multiplier == 0 {
			multiplier = 1
		}
		total += multiplier * ((float64(normalInput)/1_000_000)*price.Input +
			(float64(cacheRead)/1_000_000)*price.CacheRead +
			(float64(cacheWrite)/1_000_000)*price.CacheWrite +
			(float64(maxInt64(usage.OutputTokens, 0))/1_000_000)*price.Output)
	}
	return total, true
}

func (s *runtimeState) bindReservationLocked(requestID, authID string) {
	requestID, authID = strings.TrimSpace(requestID), strings.TrimSpace(authID)
	if requestID == "" || authID == "" {
		return
	}
	if old, ok := s.requests[requestID]; ok && old == authID {
		return
	}
	if old, ok := s.requests[requestID]; ok {
		s.releaseLocked(old)
		delete(s.requests, requestID)
	}
	for i, item := range s.pending {
		if item.AuthID == authID {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			s.requests[requestID] = authID
			return
		}
	}
}

func (s *runtimeState) releasePendingLocked(authID string) {
	for i, item := range s.pending {
		if item.AuthID == authID {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			s.releaseLocked(authID)
			return
		}
	}
}

func (s *runtimeState) releaseLocked(authID string) {
	if s.active[authID] > 0 {
		s.active[authID]--
	}
}

func loadStateLocked(path string, defaultCapacity int) error {
	state.accounts = make(map[string]accountConfig)
	state.usage = make(map[string]usageStats)
	state.usageByModel = make(map[string]map[string]tokenUsage)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state file: %w", err)
	}
	var saved persistedState
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("decode state file: %w", err)
	}
	for id, cfg := range saved.Accounts {
		if cfg.Capacity < 1 {
			cfg.Capacity = defaultCapacity
		}
		state.accounts[id] = cfg
	}
	for id, item := range saved.Usage {
		state.usage[id] = item
	}
	for id, models := range saved.UsageByModel {
		state.usageByModel[id] = models
	}
	return nil
}

func saveStateLocked() error {
	path := state.cfg.StateFile
	if path == "" {
		return nil
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	saved := persistedState{Accounts: state.accounts, Usage: state.usage, UsageByModel: state.usageByModel}
	data, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}
func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}
func schedulerError(status int, message string) ([]byte, error) {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: "capacity_full", Message: message, Retryable: true, HTTPStatus: status}})
	return raw, nil
}
func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
func metadataString(meta map[string]any, key string) string {
	if value, ok := meta[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}
func jsonResponse(status int, value any) pluginapi.ManagementResponse {
	raw, _ := json.Marshal(value)
	return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"content-type": []string{"application/json; charset=utf-8"}}, Body: raw}
}
func htmlResponse(status int, body string) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"content-type": []string{"text/html; charset=utf-8"}}, Body: []byte(body)}
}

//go:embed dashboard.html
var dashboardHTMLTemplate string

func dashboardHTML() string {
	state.mu.Lock()
	key := state.cfg.CPAManagementKey
	state.mu.Unlock()
	keyJSON, _ := json.Marshal(key)
	return strings.ReplaceAll(dashboardHTMLTemplate, "__CPA_MANAGEMENT_KEY_JSON__", string(keyJSON))
}

func legacyDashboardHTML() string {
	return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>CPA 账号并发管理</title><style>
:root{color-scheme:dark;--bg:#101418;--panel:#171d23;--line:#2a333c;--text:#edf2f7;--muted:#98a6b5;--accent:#56c596;--danger:#ff8f8f}*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}main{max-width:1500px;margin:0 auto;padding:28px}.top{display:flex;gap:12px;align-items:end;justify-content:space-between;flex-wrap:wrap}.title h1{margin:0 0 6px;font-size:24px}.title p{margin:0;color:var(--muted)}.controls{display:flex;gap:8px;flex-wrap:wrap}.controls input{min-width:310px}.input,button{border:1px solid var(--line);border-radius:6px;background:#11171c;color:var(--text);padding:9px 11px;font:inherit}button{cursor:pointer;background:#20352e;border-color:#316b55}button:hover{background:#29483d}.stats{display:grid;grid-template-columns:repeat(6,minmax(120px,1fr));gap:10px;margin:24px 0}.stat{background:var(--panel);border:1px solid var(--line);padding:15px}.stat b{display:block;font-size:21px;margin-top:5px}.stat span{color:var(--muted);font-size:12px}.table-wrap{overflow:auto;border:1px solid var(--line);background:var(--panel)}table{width:100%;border-collapse:collapse;min-width:1100px}th,td{text-align:left;border-bottom:1px solid var(--line);padding:12px 10px;white-space:nowrap}th{color:var(--muted);font-size:12px;font-weight:500}td small{display:block;color:var(--muted);margin-top:3px}.pill{display:inline-block;padding:3px 7px;border-radius:4px;background:#234e3d;color:#a6ebc9;font-size:12px}.off{background:#493333;color:#ffc0c0}.muted{color:var(--muted)}.err{color:var(--danger);margin-top:12px;white-space:pre-wrap}@media(max-width:900px){main{padding:18px}.stats{grid-template-columns:repeat(2,1fr)}.controls input{min-width:220px}}
</style></head><body><main><div class="top"><div class="title"><h1>CPA 账号并发管理</h1><p>容量 = 单个认证账号允许的最大同时请求数；费用按 Models.dev 价格表计算</p></div><div class="controls"><input id="key" class="input" type="password" placeholder="CPA Management Key"><button onclick="loadData()">刷新</button><button onclick="resetUsage()">清空用量</button></div></div><div id="err" class="err"></div><section id="stats" class="stats"></section><div class="table-wrap"><table><thead><tr><th>账号</th><th>平台</th><th>状态</th><th>容量</th><th>当前并发</th><th>请求</th><th>成功率</th><th>Token</th><th>费用</th><th>操作</th></tr></thead><tbody id="rows"><tr><td colspan="10" class="muted">请输入 CPA Management Key 后刷新</td></tr></tbody></table></div></main><script>
const keyEl=document.getElementById('key');keyEl.value=sessionStorage.getItem('cpa-management-key')||'';
function headers(){return {'X-Management-Key':keyEl.value,'Content-Type':'application/json'}}
function esc(x){return String(x??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
function fmt(n){return Number(n||0).toLocaleString()}
function pct(a,b){return b?((a/b)*100).toFixed(1)+'%':'-'}
async function loadData(){const k=keyEl.value.trim();if(!k){document.getElementById('err').textContent='请先填写 CPA Management Key';return}sessionStorage.setItem('cpa-management-key',k);try{const r=await fetch('/v0/management/account-capacity/accounts',{headers:headers()});if(!r.ok)throw new Error('HTTP '+r.status+'：管理密钥无效或 CPA 管理接口不可用');const d=await r.json();render(d)}catch(e){document.getElementById('err').textContent=e.message}}
function render(d){document.getElementById('err').textContent='';const t=d.totals||{};document.getElementById('stats').innerHTML=[['账号',t.accounts],['已启用',t.enabled],['当前并发',t.active],['总容量',t.capacity],['总请求',fmt(t.requests)],['总 Token',fmt(t.total_tokens)]].map(x=>'<div class="stat"><span>'+x[0]+'</span><b>'+x[1]+'</b></div>').join('');document.getElementById('rows').innerHTML=(d.accounts||[]).map(a=>{const u=a.usage||{};const label=a.label||a.email||a.name||a.id;return '<tr><td>'+esc(label)+'<small>'+esc(a.id)+'</small></td><td>'+esc(a.provider||a.type)+'</td><td><span class="pill '+((!a.enabled||a.disabled||a.unavailable)?'off':'')+'">'+((!a.enabled||a.disabled)?'停用':a.unavailable?'不可用':'启用')+'</span></td><td><input class="input cap" data-id="'+esc(a.id)+'" type="number" min="1" value="'+a.capacity+'" style="width:80px"></td><td>'+a.active+' / '+a.capacity+'</td><td>'+fmt(u.requests)+'</td><td>'+pct(u.success,u.requests)+'</td><td>'+fmt(u.total_tokens)+'</td><td>$'+Number(u.cost_usd||0).toFixed(4)+'</td><td><button onclick="save(\''+encodeURIComponent(a.id)+'\')">保存</button> <button onclick="toggle(\''+encodeURIComponent(a.id)+'\','+(!a.enabled)+')">'+(a.enabled?'停用':'启用')+'</button></td></tr>'}).join('')||'<tr><td colspan="10" class="muted">没有找到 CPA 认证账号</td></tr>'}
async function save(id){id=decodeURIComponent(id);const input=document.querySelector('.cap[data-id="'+CSS.escape(id)+'"]');await update({id,capacity:Number(input.value)})}
async function toggle(id,en){await update({id:decodeURIComponent(id),capacity:Number(document.querySelector('.cap[data-id="'+CSS.escape(decodeURIComponent(id))+'"]').value),enabled:en})}
async function update(body){try{const r=await fetch('/v0/management/account-capacity/accounts',{method:'PUT',headers:headers(),body:JSON.stringify(body)});if(!r.ok)throw new Error('保存失败：HTTP '+r.status);await loadData()}catch(e){document.getElementById('err').textContent=e.message}}
async function resetUsage(){if(!confirm('确定清空全部账号用量统计吗？'))return;try{const r=await fetch('/v0/management/account-capacity/reset-usage',{method:'POST',headers:headers()});if(!r.ok)throw new Error('清空失败：HTTP '+r.status);await loadData()}catch(e){document.getElementById('err').textContent=e.message}}
</script></body></html>`
}
