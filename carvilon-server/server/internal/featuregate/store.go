package featuregate

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Store is the DB seam for the feature-gating tables (license,
// license_features, templates, template_features, viewer_feature_exposure and
// viewers.template_id). It produces the immutable Snapshot the pure Resolve
// function consumes, and carries the slim seed/admin helpers used by tests and
// manual setup (no UI in this step). Stateless over *sql.DB.
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
// and its per-viewer exposure overrides for one resolution pass.
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
		return snap, nil // viewer vanished mid-request: license-only still resolves
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
		`SELECT feature_key, exposure, value FROM template_features WHERE template_id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("featuregate: load template_features: %w", err)
	}
	defer rows.Close()
	t := &Template{exposure: map[string]string{}, value: map[string]string{}}
	for rows.Next() {
		var key string
		var exposure, value sql.NullString
		if err := rows.Scan(&key, &exposure, &value); err != nil {
			return nil, fmt.Errorf("featuregate: scan template_features: %w", err)
		}
		if exposure.Valid {
			t.exposure[key] = exposure.String
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

func (s *Store) loadOverrides(ctx context.Context, mac string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT feature_key, exposure FROM viewer_feature_exposure WHERE viewer_mac = ?`, mac)
	if err != nil {
		return nil, fmt.Errorf("featuregate: load viewer_feature_exposure: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, exposure string
		if err := rows.Scan(&key, &exposure); err != nil {
			return nil, fmt.Errorf("featuregate: scan viewer_feature_exposure: %w", err)
		}
		out[key] = exposure
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("featuregate: viewer_feature_exposure rows: %w", err)
	}
	return out, nil
}

// ViewersByTemplate returns the MACs of every viewer attached to templateID,
// for fanning a template change out over the per-MAC config.changed bus.
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

// SetTemplateFeature upserts one template_features cell. exposure/value nil =
// stored as NULL (= inherit that axis). A non-nil exposure is validated.
func (s *Store) SetTemplateFeature(ctx context.Context, templateID int64, key string, exposure, value *string) error {
	if exposure != nil && !ValidExposure(*exposure) {
		return fmt.Errorf("featuregate: set template feature: invalid exposure %q", *exposure)
	}
	var ev, vv any
	if exposure != nil {
		ev = *exposure
	}
	if value != nil {
		vv = *value
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO template_features (template_id, feature_key, exposure, value)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(template_id, feature_key) DO UPDATE SET
		    exposure = excluded.exposure,
		    value    = excluded.value`,
		templateID, key, ev, vv)
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

// SetViewerExposure upserts the per-viewer exposure override. exposure is
// validated against the known set (admin/seed path).
func (s *Store) SetViewerExposure(ctx context.Context, mac, key, exposure string) error {
	if !ValidExposure(exposure) {
		return fmt.Errorf("featuregate: set viewer exposure: invalid exposure %q", exposure)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO viewer_feature_exposure (viewer_mac, feature_key, exposure) VALUES (?, ?, ?)
		ON CONFLICT(viewer_mac, feature_key) DO UPDATE SET exposure = excluded.exposure`,
		mac, key, exposure)
	if err != nil {
		return fmt.Errorf("featuregate: set viewer exposure: %w", err)
	}
	return nil
}

// ClearViewerExposure removes the per-viewer override (back to default
// tenant_visible).
func (s *Store) ClearViewerExposure(ctx context.Context, mac, key string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM viewer_feature_exposure WHERE viewer_mac = ? AND feature_key = ?`, mac, key)
	if err != nil {
		return fmt.Errorf("featuregate: clear viewer exposure: %w", err)
	}
	return nil
}

// --- Admin read helpers (Saison 20 viewer-settings page) ---

// LicenseInfo is the singleton license/abo record, surfaced read-only for the
// admin Abo frame. ViewerLimit nil = unlimited; ValidUntil nil = perpetual.
type LicenseInfo struct {
	PlanName    string
	ViewerLimit *int
	ValidUntil  *int64 // unix millis
	UpdatedAt   int64
}

// GetLicense returns the singleton license record (id=1), or (nil, nil) when
// none has been seeded yet (fresh install) so the page shows "kein Abo".
func (s *Store) GetLicense(ctx context.Context) (*LicenseInfo, error) {
	var (
		plan       string
		limit      sql.NullInt64
		validUntil sql.NullInt64
		updatedAt  int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT plan_name, viewer_limit, valid_until, updated_at FROM license WHERE id = 1`).
		Scan(&plan, &limit, &validUntil, &updatedAt)
	switch {
	case err == sql.ErrNoRows:
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("featuregate: get license: %w", err)
	}
	out := &LicenseInfo{PlanName: plan, UpdatedAt: updatedAt}
	if limit.Valid {
		v := int(limit.Int64)
		out.ViewerLimit = &v
	}
	if validUntil.Valid {
		v := validUntil.Int64
		out.ValidUntil = &v
	}
	return out, nil
}

// CountViewers returns the number of adopted viewers, for the Abo "n / Limit"
// display.
func (s *Store) CountViewers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM viewers`).Scan(&n); err != nil {
		return 0, fmt.Errorf("featuregate: count viewers: %w", err)
	}
	return n, nil
}

// TemplateInfo is one row for the admin "Vorlage zuweisen" dropdown.
type TemplateInfo struct {
	ID   int64
	Name string
}

// ListTemplates returns every template (id + name) ordered by name for the
// admin assignment dropdown.
func (s *Store) ListTemplates(ctx context.Context) ([]TemplateInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM templates ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("featuregate: list templates: %w", err)
	}
	defer rows.Close()
	var out []TemplateInfo
	for rows.Next() {
		var t TemplateInfo
		if err := rows.Scan(&t.ID, &t.Name); err != nil {
			return nil, fmt.Errorf("featuregate: scan template: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ViewerTemplate returns the viewer's currently assigned template (id + name).
// found=false when the viewer has no template (NULL) or vanished.
func (s *Store) ViewerTemplate(ctx context.Context, mac string) (id int64, name string, found bool, err error) {
	var tid sql.NullInt64
	e := s.db.QueryRowContext(ctx, `SELECT template_id FROM viewers WHERE mac = ?`, mac).Scan(&tid)
	switch {
	case e == sql.ErrNoRows:
		return 0, "", false, nil
	case e != nil:
		return 0, "", false, fmt.Errorf("featuregate: viewer template_id: %w", e)
	}
	if !tid.Valid {
		return 0, "", false, nil
	}
	e = s.db.QueryRowContext(ctx, `SELECT name FROM templates WHERE id = ?`, tid.Int64).Scan(&name)
	switch {
	case e == sql.ErrNoRows:
		return tid.Int64, "", true, nil // assigned id but name gone (shouldn't happen)
	case e != nil:
		return 0, "", false, fmt.Errorf("featuregate: template name: %w", e)
	}
	return tid.Int64, name, true, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
