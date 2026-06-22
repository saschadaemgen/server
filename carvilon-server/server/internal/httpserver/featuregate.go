package httpserver

import (
	"context"

	"carvilon.local/server/internal/featuregate"
	"carvilon.local/server/internal/viewermanager"
)

// resolveFeatureGates resolves the full feature catalog for one viewer. It
// returns (nil, nil) when the feature store is unwired so callers stay
// backwards compatible (no gating block, plain Resolve*() values). An error is
// returned (not fatal) so the caller can log and fall back to Resolve*().
func (s *Server) resolveFeatureGates(ctx context.Context, info *viewermanager.ViewerInfo) (map[string]featuregate.Effective, error) {
	if s.features == nil || info == nil {
		return nil, nil
	}
	snap, err := s.features.SnapshotForViewer(ctx, info.MAC)
	if err != nil {
		return nil, err
	}
	return featuregate.ResolveAll(featuregate.DefaultCatalog(), snap, info), nil
}

// broadcastConfigChangedToTenant fans config.changed to every device of the
// tenant (TenantSiblingMACs; today = just this one device). There is NO
// server-side exclusion of the writing device - for the Android FCM case it is
// not expressible - so the writer drops its own echo CLIENT-side, exactly like
// the web viewer's CONFIG_ECHO_SKIP_MS window. The fan-out is degrading: with
// no tenant group it hits only the writer (which discards the echo).
func (s *Server) broadcastConfigChangedToTenant(ctx context.Context, mac string) {
	if s.hub == nil {
		return
	}
	macs, err := s.viewerMgr.TenantSiblingMACs(ctx, mac)
	if err != nil {
		s.log.Warn("featuregate: tenant fan-out", "err", err, "mac_prefix", safePrefix(mac))
		macs = []string{mac} // degrade to just the writer
	}
	for _, m := range macs {
		s.hub.BroadcastConfigChanged(ctx, m)
	}
}

// broadcastTemplateChanged fans a template change out to every attached viewer
// over the existing per-MAC config.changed bus (signal-only: the viewer
// re-fetches /esp/config or /webviewer/settings.json and re-resolves live - no
// copy on attach). Safe to call with an unwired store/hub. Reused by a future
// template-edit handler; there is no UI in this step.
func (s *Server) broadcastTemplateChanged(ctx context.Context, templateID int64) {
	if s.features == nil || s.hub == nil {
		return
	}
	macs, err := s.features.ViewersByTemplate(ctx, templateID)
	if err != nil {
		s.log.Warn("featuregate: template-change fan-out", "err", err, "template_id", templateID)
		return
	}
	for _, mac := range macs {
		s.hub.BroadcastConfigChanged(ctx, mac)
	}
}
