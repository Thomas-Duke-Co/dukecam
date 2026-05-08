package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ─── Fyxt API Client ────────────────────────────────────────────
// Fetches users/inspectors from the Fyxt property management API.
// Includes in-memory caching with configurable TTL.

// FyxtUser represents a user from the Fyxt API.
type FyxtUser struct {
	ID        string   `json:"id"`
	FirstName string   `json:"first_name"`
	LastName  string   `json:"last_name"`
	Email     string   `json:"email"`
	Category  string   `json:"category"`
	Phone     string   `json:"phone"`
	IsActive  bool     `json:"is_active"`
	Types     []string `json:"types"`
	Groups    []string `json:"group_names"`
	Photo     string   `json:"photo"`
}

// FullName returns "First Last" with trimming.
func (u FyxtUser) FullName() string {
	name := u.FirstName
	if u.LastName != "" {
		if name != "" {
			name += " "
		}
		name += u.LastName
	}
	if name == "" {
		return u.Email
	}
	return name
}

// Initials returns up to 2 initials from the user's name.
func (u FyxtUser) Initials() string {
	parts := strings.Fields(u.FullName())
	if len(parts) == 0 {
		return "?"
	}
	result := string([]rune(parts[0])[0:1])
	if len(parts) > 1 {
		result += string([]rune(parts[len(parts)-1])[0:1])
	}
	return strings.ToUpper(result)
}

// GroupLabel returns the first group name or "Staff".
func (u FyxtUser) GroupLabel() string {
	if len(u.Groups) > 0 {
		return u.Groups[0]
	}
	return "Staff"
}

// FyxtClient talks to the Fyxt open API (https://open.apifyxt.com).
type FyxtClient struct {
	baseURL    string
	apiKey     string
	schema     string
	httpClient *http.Client
	cacheTTL   time.Duration

	mu        sync.RWMutex
	users     []FyxtUser
	userMap   map[string]FyxtUser // keyed by ID
	cachedAt  time.Time
}

// NewFyxtClient creates a Fyxt API client with sensible defaults.
func NewFyxtClient(baseURL, apiKey, schema string) *FyxtClient {
	return &FyxtClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		schema:  schema,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
		cacheTTL: 10 * time.Minute,
		userMap:  make(map[string]FyxtUser),
	}
}

// fyxtResponse is the standard Fyxt API envelope.
type fyxtResponse struct {
	Status     bool            `json:"status"`
	StatusCode int             `json:"status_code"`
	Errors     json.RawMessage `json:"errors"`
	Data       *fyxtData       `json:"data"`
}

type fyxtData struct {
	Data  json.RawMessage `json:"data"`
	Total int             `json:"total"`
	Count int             `json:"count"` // some endpoints use "count" instead of "total"
}

// get performs an authenticated GET request to the Fyxt API.
func (fc *FyxtClient) get(ctx context.Context, endpoint string, params map[string]string) (*fyxtResponse, error) {
	url := fc.baseURL + endpoint

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("fyxt: build request: %w", err)
	}

	req.Header.Set("x-api-key", fc.apiKey)
	req.Header.Set("x-schema", fc.schema)

	if len(params) > 0 {
		q := req.URL.Query()
		for k, v := range params {
			q.Set(k, v)
		}
		req.URL.RawQuery = q.Encode()
	}

	resp, err := fc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fyxt: request %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fyxt: %s returned HTTP %d", endpoint, resp.StatusCode)
	}

	var result fyxtResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("fyxt: decode response: %w", err)
	}

	if !result.Status {
		return nil, fmt.Errorf("fyxt: API error on %s: %s", endpoint, string(result.Errors))
	}

	return &result, nil
}

// FetchUsers fetches all users from Fyxt with pagination.
// Results are cached in memory for cacheTTL duration.
func (fc *FyxtClient) FetchUsers(ctx context.Context) ([]FyxtUser, error) {
	// Check cache first
	fc.mu.RLock()
	if !fc.cachedAt.IsZero() && time.Since(fc.cachedAt) < fc.cacheTTL {
		users := make([]FyxtUser, len(fc.users))
		copy(users, fc.users)
		fc.mu.RUnlock()
		return users, nil
	}
	fc.mu.RUnlock()

	// Fetch all pages
	var allUsers []FyxtUser
	skip := 0
	limit := 200

	for {
		result, err := fc.get(ctx, "/v1/user", map[string]string{
			"limit": fmt.Sprintf("%d", limit),
			"skip":  fmt.Sprintf("%d", skip),
		})
		if err != nil {
			return nil, err
		}

		if result.Data == nil {
			break
		}

		var users []FyxtUser
		if err := json.Unmarshal(result.Data.Data, &users); err != nil {
			return nil, fmt.Errorf("fyxt: unmarshal users: %w", err)
		}

		allUsers = append(allUsers, users...)

		// Determine total — Fyxt uses "total" or "count"
		total := result.Data.Total
		if total == 0 {
			total = result.Data.Count
		}

		skip += limit
		if total == 0 || skip >= total {
			break
		}
	}

	// Update cache
	fc.mu.Lock()
	fc.users = allUsers
	fc.userMap = make(map[string]FyxtUser, len(allUsers))
	for _, u := range allUsers {
		fc.userMap[u.ID] = u
	}
	fc.cachedAt = time.Now()
	fc.mu.Unlock()

	log.Printf("Fyxt: cached %d users", len(allUsers))

	// Return a copy
	out := make([]FyxtUser, len(allUsers))
	copy(out, allUsers)
	return out, nil
}

// GetUser returns a single user by ID from cache, fetching if needed.
func (fc *FyxtClient) GetUser(ctx context.Context, id string) (*FyxtUser, error) {
	// Try cache first
	fc.mu.RLock()
	if !fc.cachedAt.IsZero() && time.Since(fc.cachedAt) < fc.cacheTTL {
		if u, ok := fc.userMap[id]; ok {
			fc.mu.RUnlock()
			return &u, nil
		}
		fc.mu.RUnlock()
		return nil, fmt.Errorf("fyxt: user %s not found", id)
	}
	fc.mu.RUnlock()

	// Cache expired or empty — refresh
	users, err := fc.FetchUsers(ctx)
	if err != nil {
		return nil, err
	}

	for _, u := range users {
		if u.ID == id {
			return &u, nil
		}
	}
	return nil, fmt.Errorf("fyxt: user %s not found", id)
}

// GetInspectors returns users filtered to likely inspectors (active company staff).
// In Fyxt, Thomas Duke staff are category="Customer" with types=["Manager"].
// Tenants and vendors are excluded.
func (fc *FyxtClient) GetInspectors(ctx context.Context) ([]FyxtUser, error) {
	users, err := fc.FetchUsers(ctx)
	if err != nil {
		return nil, err
	}

	var inspectors []FyxtUser
	for _, u := range users {
		if !u.IsActive {
			continue
		}
		// Include active "Customer" category users (company staff) as potential inspectors.
		// Exclude Tenant and Vendor categories.
		cat := strings.ToLower(u.Category)
		if cat == "customer" {
			inspectors = append(inspectors, u)
		}
	}
	return inspectors, nil
}

// SearchInspectors returns inspectors matching a search query (case-insensitive name/email/group match).
func (fc *FyxtClient) SearchInspectors(ctx context.Context, query string) ([]FyxtUser, error) {
	inspectors, err := fc.GetInspectors(ctx)
	if err != nil {
		return nil, err
	}

	if query == "" {
		return inspectors, nil
	}

	q := strings.ToLower(strings.TrimSpace(query))
	var matched []FyxtUser
	for _, u := range inspectors {
		name := strings.ToLower(u.FullName())
		email := strings.ToLower(u.Email)
		group := strings.ToLower(u.GroupLabel())
		if strings.Contains(name, q) || strings.Contains(email, q) || strings.Contains(group, q) {
			matched = append(matched, u)
		}
	}
	return matched, nil
}

// InvalidateCache forces the next call to re-fetch from the API.
func (fc *FyxtClient) InvalidateCache() {
	fc.mu.Lock()
	fc.cachedAt = time.Time{}
	fc.mu.Unlock()
}

// IsConfigured returns true if the client has API credentials set.
func (fc *FyxtClient) IsConfigured() bool {
	return fc.apiKey != "" && fc.schema != ""
}

// ─── Work Order (Job) Creation ──────────────────────────────────

// ─── Fyxt Unit Lookup ───────────────────────────────────────────

// FyxtUnit is a unit/space record from the Fyxt API.
type FyxtUnit struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// fyxtUnitResponse is the envelope for the unit list endpoint.
type fyxtUnitResponse struct {
	Status bool `json:"status"`
	Data   struct {
		Data  []FyxtUnit `json:"data"`
		Total int        `json:"total"`
		Count int        `json:"count"`
	} `json:"data"`
}

// GetDefaultUnitForProperty returns the best default unit for a work order.
// Priority: "Full Building" > "Building Interior" > first unit.
// Returns empty string if no units found.
func (fc *FyxtClient) GetDefaultUnitForProperty(ctx context.Context, propertyID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fc.baseURL+"/v1/unit", nil)
	if err != nil {
		return "", fmt.Errorf("fyxt: build unit request: %w", err)
	}
	req.Header.Set("x-api-key", fc.apiKey)
	req.Header.Set("x-schema", fc.schema)
	q := req.URL.Query()
	q.Set("property_id", propertyID)
	q.Set("limit", "50")
	req.URL.RawQuery = q.Encode()

	resp, err := fc.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fyxt: units request: %w", err)
	}
	defer resp.Body.Close()

	var result fyxtUnitResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("fyxt: decode units: %w", err)
	}

	units := result.Data.Data
	if len(units) == 0 {
		return "", nil
	}

	// Preference order for a generic "whole building" unit
	preferred := []string{"full building", "building interior", "entire property", "common area"}
	for _, want := range preferred {
		for _, u := range units {
			if strings.EqualFold(u.Name, want) {
				return u.ID, nil
			}
		}
	}
	// Fall back to first unit
	return units[0].ID, nil
}

// WorkOrderPayload is the request body for creating a Fyxt work order (job).
type WorkOrderPayload struct {
	PropertyID      string  `json:"property_id"`
	UnitID          string  `json:"unit_id"`           // required by Fyxt API
	Address         string  `json:"address"`
	Description     string  `json:"description"`
	Priorities      string  `json:"priorities"`        // "Low", "Medium", "High", "Emergency"
	Type            string  `json:"type"`              // "Regular"
	RequestType     string  `json:"request_type"`      // "Common Area", "In Unit"
	Status          string  `json:"status"`            // "New"
	StatusChangedOn string  `json:"status_changed_on"` // ISO timestamp
	Responsible     string  `json:"responsible"`       // "Manager"
	Source          string  `json:"source"`
	SkipBid         bool    `json:"skip_bid"`
	NotifyTenant    bool    `json:"notify_tenant"`
	TargetDate      *string `json:"target_date,omitempty"`
}

// WorkOrderResult is the job record returned by Fyxt on creation.
type WorkOrderResult struct {
	ID                 string  `json:"id"`
	ExternalIdentifier *string `json:"external_identifier"`
}

// fyxtCreateJobResponse is the response envelope for the POST /v1/job endpoint.
type fyxtCreateJobResponse struct {
	Status     bool   `json:"status"`
	StatusCode int    `json:"status_code"`
	Data       struct {
		RecordsCount int               `json:"records_count"`
		FailedCount  int               `json:"failed_count"`
		Records      []WorkOrderResult `json:"records"`
		Errors       []string          `json:"errors"`
	} `json:"data"`
}

// postRaw performs an authenticated POST request and returns the raw response body.
func (fc *FyxtClient) postRaw(ctx context.Context, endpoint string, body interface{}) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("fyxt: marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fc.baseURL+endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("fyxt: build request: %w", err)
	}
	req.Header.Set("x-api-key", fc.apiKey)
	req.Header.Set("x-schema", fc.schema)
	req.Header.Set("Content-Type", "application/json")

	resp, err := fc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fyxt: request %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fyxt: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("fyxt: %s returned HTTP %d: %s", endpoint, resp.StatusCode, string(raw))
	}
	return raw, nil
}

// CreateWorkOrder creates a new work order (job) in Fyxt from an inspection.
func (fc *FyxtClient) CreateWorkOrder(ctx context.Context, payload WorkOrderPayload) (*WorkOrderResult, error) {
	if payload.StatusChangedOn == "" {
		payload.StatusChangedOn = time.Now().UTC().Format(time.RFC3339)
	}
	if payload.Status == "" {
		payload.Status = "New"
	}
	if payload.Type == "" {
		payload.Type = "Regular"
	}
	if payload.Responsible == "" {
		payload.Responsible = "Manager"
	}
	if payload.Source == "" {
		payload.Source = "DukeCam Inspection"
	}

	raw, err := fc.postRaw(ctx, "/v1/job", payload)
	if err != nil {
		return nil, err
	}

	var result fyxtCreateJobResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("fyxt: decode job response: %w", err)
	}

	if !result.Status {
		return nil, fmt.Errorf("fyxt: API rejected work order creation")
	}
	if result.Data.FailedCount > 0 || len(result.Data.Errors) > 0 {
		errMsg := "unknown error"
		if len(result.Data.Errors) > 0 {
			errMsg = result.Data.Errors[0]
		}
		return nil, fmt.Errorf("fyxt: work order creation failed: %s", errMsg)
	}
	if len(result.Data.Records) == 0 {
		return nil, fmt.Errorf("fyxt: no record returned")
	}

	log.Printf("Fyxt: created work order #%s", result.Data.Records[0].ID)
	return &result.Data.Records[0], nil
}
