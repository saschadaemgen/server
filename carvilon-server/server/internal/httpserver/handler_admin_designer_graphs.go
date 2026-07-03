package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"carvilon.local/server/internal/designerstore"
)

// Designer folder-tree + graph persistence (migration 032). All routes
// sit behind requireAdminSession next to the other /a/designer/ JSON
// endpoints; the more specific patterns win over the static bundle
// subtree. Mutations of protected system folders map to 403, deleting
// a non-empty folder to 409 - the store owns those rules, this file
// only translates errors to status codes.

// designerGraphMaxBytes caps a saved graph body, mirroring the run
// endpoint's 1 MB graph cap.
const designerGraphMaxBytes = 1 << 20

// designerStoreErr translates store sentinel errors into the JSON
// error reply convention ({"ok":false,"error":...}).
func (s *Server) designerStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, designerstore.ErrSystemFolder):
		designerJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "system folder is protected"})
	case errors.Is(err, designerstore.ErrFolderNotEmpty):
		designerJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "folder not empty"})
	case errors.Is(err, designerstore.ErrNotFound):
		designerJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "not found"})
	case errors.Is(err, designerstore.ErrEmptyName):
		designerJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "name must not be empty"})
	default:
		s.log.Error("designer store", "err", err)
		designerJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
	}
}

// designerStoreReady nil-checks the store (no DB in exotic setups) so
// every handler can rely on it afterwards.
func (s *Server) designerStoreReady(w http.ResponseWriter) bool {
	if s.designerStore == nil {
		designerJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "designer storage unavailable"})
		return false
	}
	return true
}

// designerPathID parses the {id} path segment.
func designerPathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		designerJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid id"})
		return 0, false
	}
	return id, true
}

// handleDesignerTree serves the full folder/graph tree as flat,
// pre-sorted lists the editor assembles.
// Route: GET /a/designer/tree (requireAdminSession).
func (s *Server) handleDesignerTree(w http.ResponseWriter, r *http.Request) {
	if !s.designerStoreReady(w) {
		return
	}
	folders, graphs, err := s.designerStore.Tree(r.Context())
	if err != nil {
		s.designerStoreErr(w, err)
		return
	}
	if folders == nil {
		folders = []designerstore.Folder{}
	}
	if graphs == nil {
		graphs = []designerstore.GraphMeta{}
	}
	designerJSON(w, http.StatusOK, map[string]any{"folders": folders, "graphs": graphs})
}

// handleDesignerFolderCreate creates a folder. Body: {parent_id, name}
// (parent_id 0/absent = root). Route: POST /a/designer/folders.
func (s *Server) handleDesignerFolderCreate(w http.ResponseWriter, r *http.Request) {
	if !s.designerStoreReady(w) {
		return
	}
	var req struct {
		ParentID int64  `json:"parent_id"`
		Name     string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		designerJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	f, err := s.designerStore.CreateFolder(r.Context(), req.ParentID, req.Name)
	if err != nil {
		s.designerStoreErr(w, err)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"ok": true, "folder": f})
}

// handleDesignerFolderRename renames a folder. Body: {name}.
// Route: POST /a/designer/folders/{id}/rename.
func (s *Server) handleDesignerFolderRename(w http.ResponseWriter, r *http.Request) {
	if !s.designerStoreReady(w) {
		return
	}
	id, ok := designerPathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		designerJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	if err := s.designerStore.RenameFolder(r.Context(), id, req.Name); err != nil {
		s.designerStoreErr(w, err)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDesignerFolderDelete deletes an empty, non-system folder.
// Route: POST /a/designer/folders/{id}/delete.
func (s *Server) handleDesignerFolderDelete(w http.ResponseWriter, r *http.Request) {
	if !s.designerStoreReady(w) {
		return
	}
	id, ok := designerPathID(w, r)
	if !ok {
		return
	}
	if err := s.designerStore.DeleteFolder(r.Context(), id); err != nil {
		s.designerStoreErr(w, err)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDesignerGraphCreate creates an empty graph. Body:
// {folder_id, name}. Route: POST /a/designer/graphs.
func (s *Server) handleDesignerGraphCreate(w http.ResponseWriter, r *http.Request) {
	if !s.designerStoreReady(w) {
		return
	}
	var req struct {
		FolderID int64  `json:"folder_id"`
		Name     string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		designerJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	g, err := s.designerStore.CreateGraph(r.Context(), req.FolderID, req.Name)
	if err != nil {
		s.designerStoreErr(w, err)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"ok": true, "graph": g})
}

// handleDesignerGraphGet loads one graph including its content. The
// stored JSON is embedded verbatim (it was validated on save).
// Route: GET /a/designer/graphs/{id}.
func (s *Server) handleDesignerGraphGet(w http.ResponseWriter, r *http.Request) {
	if !s.designerStoreReady(w) {
		return
	}
	id, ok := designerPathID(w, r)
	if !ok {
		return
	}
	g, err := s.designerStore.Graph(r.Context(), id)
	if err != nil {
		s.designerStoreErr(w, err)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{
		"id": g.ID, "folder_id": g.FolderID, "name": g.Name,
		"rev": g.Rev, "updated_at": g.UpdatedAt,
		"graph": json.RawMessage(g.JSON),
	})
}

// handleDesignerGraphRename renames a graph. Body: {name}.
// Route: POST /a/designer/graphs/{id}/rename.
func (s *Server) handleDesignerGraphRename(w http.ResponseWriter, r *http.Request) {
	if !s.designerStoreReady(w) {
		return
	}
	id, ok := designerPathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		designerJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	if err := s.designerStore.RenameGraph(r.Context(), id, req.Name); err != nil {
		s.designerStoreErr(w, err)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDesignerGraphDelete deletes a graph outside system folders.
// Route: POST /a/designer/graphs/{id}/delete.
func (s *Server) handleDesignerGraphDelete(w http.ResponseWriter, r *http.Request) {
	if !s.designerStoreReady(w) {
		return
	}
	id, ok := designerPathID(w, r)
	if !ok {
		return
	}
	if err := s.designerStore.DeleteGraph(r.Context(), id); err != nil {
		s.designerStoreErr(w, err)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDesignerGraphSave replaces a graph's content (the editor's
// autosave). Body: {"graph": {...}} - the graph value must be a JSON
// object; it is stored verbatim and stays opaque to the server. rev
// increments server-side; the new value is returned for the status
// bar. Route: POST /a/designer/graphs/{id}/save.
func (s *Server) handleDesignerGraphSave(w http.ResponseWriter, r *http.Request) {
	if !s.designerStoreReady(w) {
		return
	}
	id, ok := designerPathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Graph json.RawMessage `json:"graph"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, designerGraphMaxBytes)).Decode(&req); err != nil {
		designerJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	if len(req.Graph) == 0 || req.Graph[0] != '{' {
		designerJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "graph must be a JSON object"})
		return
	}
	rev, err := s.designerStore.SaveGraph(r.Context(), id, string(req.Graph))
	if err != nil {
		s.designerStoreErr(w, err)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"ok": true, "rev": rev})
}
