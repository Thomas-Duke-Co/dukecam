package main

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// POSDB is a read-only connection to the PropertyOS database, used to power the
// property/unit picker on the capture page. PropertyOS's /api/buildings/:id
// (rent-roll) route requires a user session — it returns 401 for the service
// token — so we read buildings/units directly from the shared Postgres server
// instead. Points at property_dashboard_staging, which carries both the real
// managed buildings and the Taliercio prospect buildings (data_source='taliercio_prospect').
type POSDB struct{ pool *pgxpool.Pool }

// POSProperty is a building for the property dropdown.
type POSProperty struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	City     string `json:"city"`
	Address  string `json:"address"`
	Prospect bool   `json:"prospect"`
}

// POSUnit is a suite for the unit dropdown.
type POSUnit struct {
	UnitID int    `json:"unit_id"`
	Suite  string `json:"suite"`
	Tenant string `json:"tenant"`
}

// NewPOSDB opens a read-only pool to the PropertyOS DB. Returns (nil, nil) when
// no URL is configured so the picker degrades gracefully.
func NewPOSDB(ctx context.Context, url string) (*POSDB, error) {
	if url == "" {
		return nil, nil
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &POSDB{pool: pool}, nil
}

func (p *POSDB) Close() {
	if p != nil && p.pool != nil {
		p.pool.Close()
	}
}

// ListProperties returns active buildings (real + prospect), name-sorted.
func (p *POSDB) ListProperties(ctx context.Context) ([]POSProperty, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, name, COALESCE(city,''), COALESCE(address,''),
		       (COALESCE(data_source,'') = 'taliercio_prospect') AS prospect
		FROM buildings
		WHERE active
		ORDER BY (COALESCE(data_source,'') = 'taliercio_prospect') DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []POSProperty{}
	for rows.Next() {
		var b POSProperty
		if err := rows.Scan(&b.ID, &b.Name, &b.City, &b.Address, &b.Prospect); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListUnits returns the suites for a building with the current tenant name.
func (p *POSDB) ListUnits(ctx context.Context, buildingID int) ([]POSUnit, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT u.id, u.suite, COALESCE(t.name, '')
		FROM units u
		LEFT JOIN leases l ON l.unit_id = u.id AND l.status NOT IN ('terminated','expired')
		LEFT JOIN tenants t ON t.id = l.tenant_id
		WHERE u.building_id = $1
		ORDER BY u.suite`, buildingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []POSUnit{}
	for rows.Next() {
		var u POSUnit
		if err := rows.Scan(&u.UnitID, &u.Suite, &u.Tenant); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// PropertyName returns a building's name (for get-or-create of its DukeCam project).
func (p *POSDB) PropertyName(ctx context.Context, buildingID int) (string, error) {
	var name string
	err := p.pool.QueryRow(ctx, `SELECT name FROM buildings WHERE id=$1`, buildingID).Scan(&name)
	return name, err
}
