package featuregate

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Store is the DB seam for the feature-gating tables (license,
// license_features, templates, template_features, viewer_feature_active and
// viewers.template_id). It produces the immutable Snapshot the pure Resolve
// function consumes, and carries the slim seed/admin helpers used by tests and
// manual setup (no UI in this step). Stateless over *sql.DB; safe for
// concurrent use.
type Store struct {
	db    *sql.DB
	nowMS func() int64
}

// NewStore wraps a *sql.DB (pass db.DB). Timestamps use the wall clock; they
// are bookkeeping only and never affect resolution.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db, nowMS: func() int64 { return time.Now().UnixMilli() }}
}

// SnapshotForViewer loads the license snapshot, the viewer's template (if any)
// and its per-viewer active overrides for one resolution pass.
func (s *Store) SnapshotForViewer(ctx context.Context, mac string) (Snapshot, error) {
	lic, err := s.loadLicense(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{License: lic}

	var templateID sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT template_id FROM viewers WHERE mac = ?`, mac).Scan(&templateID)
	switch {
	case err == sql.ErrNoRows:
		// Viewer vanished mid-request: a license-only snapshot still resolves.
		return snap, nil
	case err != nil:
		return Snapshot{}, fmt.Errorf("featuregate: load viewer template_id: %w", err)
	}
	if templateID.Valid {
		tmpl, err := s.loadTemplate(ctx, templateID.Int64)
		if err != nil {
			return Snapshot{}, err
		}
		snap.Template = tmpl
	}
	ov, err := s.loadOverrides(ctx, mac)
	if err != nil {
		return Snapshot{}, err
	}
	snap.Overrides = ov
	return snap, nil
}

func (s *Store) loadLicense(ctx context.Context) (License, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT feature_key, licensed FROM license_features`)
	if err != nil {
		return License{}, fmt.Errorf("featuregate: load license_features: %w", err)
	}
	defer rows.Close()
	feats := make(map[string]bool)
	for rows.Next() {
		var key string
		var licensed int64
		if err := rows.Scan(&key, &licensed); err != nil {
			return License{}, fmt.Errorf("featuregate: scan license_features: %w", err)
		}
		feats[key] = licensed != 0
	}
	if err := rows.Err(); err != nil {
		return License{}, fmt.Errorf("featuregate: license_features rows: %w", err)
	}
	return License{features: feats}, nil
}

func (s *Store) loadTemplate(ctx context.Context, id int64) (*Template, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT feature_key, active, value FROM template_features WHERE template_id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("featuregate: load template_features: %w", err)
	}
	defer rows.Close()
	t := &Template{active: map[string]bool{}, value: map[string]string{}}
	for rows.Next() {
		var key string
		var active sql.NullInt64
		var value sql.NullString
		if err := rows.Scan(&key, &active, &value); err != nil {
			return nil, fmt.Errorf("featuregate: scan template_features: %w", err)
		}
		if active.Valid {
			t.active[key] = active.Int64 != 0
		}
		if value.Valid {
			t.value[key] = value.String
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("featuregate: template_features rows: %w", err)
	}
	return t, nil
}

func (s *Store) loadOverrides(ctx context.Context, mac string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT feature_key, active FROM viewer_feature_active WHERE viewer_mac = ?`, mac)
	if err != nil {
		return nil, fmt.Errorf("featuregate: load viewer_feature_active: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var key string
		var active int64
		if err := rows.Scan(&key, &active); err != nil {
			return nil, fmt.Errorf("featuregate: scan viewer_feature_active: %w", err)
		}
		out[key] = active != 0
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("featuregate: viewer_feature_active rows: %w", err)
	}
	return out, nil
}

// ViewersByTemplate returns the MACs of every viewer attached to templateID,
// for fanning a template change out over the per-MAC config.changed bus
// (signal-only; viewers re-fetch and re-resolve live - no copy on attach).
func (s *Store) ViewersByTemplate(ctx context.Context, templateID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT mac FROM viewers WHERE template_id = ? ORDER BY mac`, templateID)
	if err != nil {
		return nil, fmt.Errorf("featuregate: viewers by template: %w", err)
	}
	defer rows.Close()
	var macs []string
	for rows.Next() {
		var mac string
		if err := rows.Scan(&mac); err != nil {
			return nil, fmt.Errorf("featuregate: scan viewers by template: %w", err)
		}
		macs = append(macs, mac)
	}
	return macs, rows.Err()
}

// --- Seed / admin helpers (tests + manual setup; no UI in this step) ---

// SetLicense upserts the singleton license record (id=1). viewerLimit/validUntil
// nil = stored as NULL (unlimited / perpetual).
func (s *Store) SetLicense(ctx context.Context, plan string, viewerLimit *int, validUntil *int64) error {
	var vl, vu any
	if viewerLimit != nil {
		vl = *viewerLimit
	}
	if validUntil != nil {
		vu = *validUntil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO license (id, plan_name, viewer_limit, valid_until, updated_at)
		VALUES (1, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		    plan_name    = excluded.plan_name,
		    viewer_limit = excluded.viewer_limit,
		    valid_until  = excluded.valid_until,
		    updated_at   = excluded.updated_at`,
		plan, vl, vu, s.nowMS())
	if err != nil {
		return fmt.Errorf("featuregate: set license: %w", err)
	}
	return nil
}

// SetLicenseFeature upserts a license_features row (the licensed override).
func (s *Store) SetLicenseFeature(ctx context.Context, key string, licensed bool) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO license_features (feature_key, licensed) VALUES (?, ?)
		ON CONFLICT(feature_key) DO UPDATE SET licensed = excluded.licensed`,
		key, boolToInt(licensed))
	if err != nil {
		return fmt.Errorf("featuregate: set license feature: %w", err)
	}
	return nil
}

// CreateTemplate inserts a new named template and returns its id.
func (s *Store) CreateTemplate(ctx context.Context, name string) (int64, error) {
	now := s.nowMS()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO templates (name, created_at, updated_at) VALUES (?, ?, ?)`, name, now, now)
	if err != nil {
		return 0, fmt.Errorf("featuregate: create template: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("featuregate: create template id: %w", err)
	}
	return id, nil
}

// SetTemplateFeature upserts one template_features cell. active/value nil =
// stored as NULL (= inherit that axis). Bumps the template's updated_at.
func (s *Store) SetTemplateFeature(ctx context.Context, templateID int64, key string, active *bool, value *string) error {
	var av, vv any
	if active != nil {
		av = boolToInt(*active)
	}
	if value != nil {
		vv = *value
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO template_features (template_id, feature_key, active, value)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(template_id, feature_key) DO UPDATE SET
		    active = excluded.active,
		    value  = excluded.value`,
		templateID, key, av, vv)
	if err != nil {
		return fmt.Errorf("featuregate: set template feature: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE templates SET updated_at = ? WHERE id = ?`, s.nowMS(), templateID); err != nil {
		return fmt.Errorf("featuregate: bump template updated_at: %w", err)
	}
	return nil
}

// AssignViewerTemplate sets (or clears, when templateID nil) viewers.template_id.
func (s *Store) AssignViewerTemplate(ctx context.Context, mac string, templateID *int64) error {
	var tv any
	if templateID != nil {
		tv = *templateID
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE viewers SET template_id = ?, updated_at = ? WHERE mac = ?`, tv, s.nowMS(), mac)
	if err != nil {
		return fmt.Errorf("featuregate: assign viewer template: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("featuregate: assign viewer template: viewer %q not found", mac)
	}
	return nil
}

// SetViewerFeatureActive upserts the per-viewer active override.
func (s *Store) SetViewerFeatureActive(ctx context.Context, mac, key string, active bool) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO viewer_feature_active (viewer_mac, feature_key, active) VALUES (?, ?, ?)
		ON CONFLICT(viewer_mac, feature_key) DO UPDATE SET active = excluded.active`,
		mac, key, boolToInt(active))
	if err != nil {
		return fmt.Errorf("featuregate: set viewer feature active: %w", err)
	}
	return nil
}

// ClearViewerFeatureActive removes the per-viewer override (back to inherit).
func (s *Store) ClearViewerFeatureActive(ctx context.Context, mac, key string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM viewer_feature_active WHERE viewer_mac = ? AND feature_key = ?`, mac, key)
	if err != nil {
		return fmt.Errorf("featuregate: clear viewer feature active: %w", err)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
