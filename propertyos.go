package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// ─── PropertyOS API Client ──────────────────────────────────────
// Fetches properties, buildings, and units from the PropertyOS API
// (properties.thomasduke.io / localhost:4125).

// PropertyOSConfig holds configuration for the PropertyOS API client.
type PropertyOSConfig struct {
	BaseURL string // e.g. "http://localhost:4125" or "https://properties.thomasduke.io"
	Token   string // Bearer token for service-to-service auth
}

// PropertyOSClient is an HTTP client for the PropertyOS API.
type PropertyOSClient struct {
	config     PropertyOSConfig
	httpClient *http.Client

	// In-memory cache with TTL to avoid hammering PropertyOS on every page load.
	mu         sync.RWMutex
	buildings  []Building
	buildingAt time.Time
	cacheTTL   time.Duration
}

// Building represents a property/building from PropertyOS.
type Building struct {
	ID              int     `json:"id"`
	Name            string  `json:"name"`
	Address         string  `json:"address"`
	City            string  `json:"city"`
	State           string  `json:"state"`
	Zip             string  `json:"zip"`
	PropertyType    string  `json:"property_type"`
	TotalRSF        json.Number `json:"total_rsf"`
	TotalUSF        json.Number `json:"total_usf"`
	Floors          int     `json:"floors"`
	YearBuilt       int     `json:"year_built"`
	ParkingSpots    int     `json:"parking_spots"`
	FyxtPropertyID  *string `json:"fyxt_property_id"`
	REISPropertyID  *int    `json:"reis_property_id"`
	Slug            string  `json:"slug"`
	BuildingNumber  string  `json:"building_number"`
	Active          bool    `json:"active"`
	PMName          *string `json:"pm_name"`
}

// BuildingDetail is the full building response including rent roll data.
type BuildingDetail struct {
	Building Building   `json:"building"`
	RentRoll []RentRow  `json:"rentRoll"`
}

// RentRow represents a single row in the rent roll (unit + tenant + lease info).
type RentRow struct {
	Suite      string  `json:"suite"`
	Floor      *int    `json:"floor"`
	UnitType   string  `json:"unit_type"`
	ActualUSF  float64 `json:"actual_usf"`
	ActualRSF  float64 `json:"actual_rsf"`
	UnitID     int     `json:"unit_id"`
	TenantID   *int    `json:"tenant_id"`
	TenantName *string `json:"tenant_name"`
	LeaseStart *string `json:"lease_start"`
	LeaseEnd   *string `json:"lease_end"`
	IsMTM      bool    `json:"is_mtm"`
}

// FullAddress returns a formatted address string for display.
func (b Building) FullAddress() string {
	if b.City != "" && b.State != "" {
		return fmt.Sprintf("%s, %s, %s %s", b.Address, b.City, b.State, b.Zip)
	}
	return b.Address
}

// NewPropertyOSClient creates a new PropertyOS API client.
func NewPropertyOSClient(cfg PropertyOSConfig) *PropertyOSClient {
	return &PropertyOSClient{
		config: cfg,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		cacheTTL: 5 * time.Minute,
	}
}

// doRequest performs an authenticated HTTP GET to PropertyOS.
func (c *PropertyOSClient) doRequest(ctx context.Context, path string) ([]byte, error) {
	url := c.config.BaseURL + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("propertyos: create request: %w", err)
	}

	if c.config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.Token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("propertyos: request %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("propertyos: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("propertyos: %s returned %d: %s", path, resp.StatusCode, string(body))
	}

	return body, nil
}

// ListBuildings fetches all buildings from PropertyOS.
// Results are cached for cacheTTL to avoid excessive API calls.
func (c *PropertyOSClient) ListBuildings(ctx context.Context) ([]Building, error) {
	// Check cache first
	c.mu.RLock()
	if c.buildings != nil && time.Since(c.buildingAt) < c.cacheTTL {
		result := make([]Building, len(c.buildings))
		copy(result, c.buildings)
		c.mu.RUnlock()
		return result, nil
	}
	c.mu.RUnlock()

	// Fetch from API
	data, err := c.doRequest(ctx, "/api/buildings")
	if err != nil {
		// If we have stale cache, return it rather than failing
		c.mu.RLock()
		if c.buildings != nil {
			log.Printf("PropertyOS API error, using stale cache: %v", err)
			result := make([]Building, len(c.buildings))
			copy(result, c.buildings)
			c.mu.RUnlock()
			return result, nil
		}
		c.mu.RUnlock()
		return nil, err
	}

	var buildings []Building
	if err := json.Unmarshal(data, &buildings); err != nil {
		return nil, fmt.Errorf("propertyos: decode buildings: %w", err)
	}

	// Update cache
	c.mu.Lock()
	c.buildings = buildings
	c.buildingAt = time.Now()
	c.mu.Unlock()

	log.Printf("PropertyOS: fetched %d buildings", len(buildings))
	return buildings, nil
}

// GetBuilding fetches a single building by ID with its rent roll (units).
func (c *PropertyOSClient) GetBuilding(ctx context.Context, id int) (*BuildingDetail, error) {
	data, err := c.doRequest(ctx, fmt.Sprintf("/api/buildings/%d", id))
	if err != nil {
		return nil, err
	}

	var detail BuildingDetail
	if err := json.Unmarshal(data, &detail); err != nil {
		return nil, fmt.Errorf("propertyos: decode building detail: %w", err)
	}

	return &detail, nil
}

// InvalidateCache clears the cached buildings list, forcing a fresh fetch.
func (c *PropertyOSClient) InvalidateCache() {
	c.mu.Lock()
	c.buildings = nil
	c.buildingAt = time.Time{}
	c.mu.Unlock()
}

// IsConfigured returns true if the PropertyOS client has a base URL set.
func (c *PropertyOSClient) IsConfigured() bool {
	return c.config.BaseURL != ""
}
