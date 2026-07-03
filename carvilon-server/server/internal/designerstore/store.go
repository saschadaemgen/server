// Package designerstore is the persistence layer for the logic
// editor's folder tree and its stored graphs (migration 032). It is
// the single SQL writer for the designer_folders and designer_graphs
// tables.
//
// Folders form one tree (parent_id NULL = root). Folders flagged
// system=1 are protected scaffolding for later component tracks
// (System > Reader for Tags/RFID): they cannot be renamed or deleted,
// and neither folders nor graphs can be created inside them manually -
// every such mutation returns ErrSystemFolder. Graphs that already
// live in a system folder (created later by their own flow) keep their
// content editable: SaveGraph works there, only rename/delete stay
// locked to that flow.
//
// graph_json is the editor's own wire format and opaque to the server;
// rev increments server-side on every save (last-write-wins - a single
// admin edits, there is no multi-user conflict handling here).
package designerstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrNotFound is returned when a folder or graph id has no row.
	ErrNotFound = errors.New("designerstore: not found")
	// ErrSystemFolder is returned for any structural mutation touching
	// a protected system folder (rename/delete it, create inside it,
	// rename/delete a graph it contains).
	ErrSystemFolder = errors.New("designerstore: system folder is protected")
	// ErrFolderNotEmpty is returned when deleting a folder that still
	// has subfolders or graphs.
	ErrFolderNotEmpty = errors.New("designerstore: folder not empty")
	// ErrEmptyName is returned when a folder or graph name is blank
	// after trimming.
	ErrEmptyName = errors.New("designerstore: name must not be empty")
)

// EmptyGraphJSON is the content of a freshly created graph, matching
// the column default in migration 032.
const EmptyGraphJSON = `{"schema":1,"nodes":[],"edges":[]}`

// maxNameLen bounds folder and graph names (runes, not bytes).
const maxNameLen = 120

// Store is the SQL gateway for the designer folder tree and graphs.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Option mutates a Store during construction.
type Option func(*Store)

// WithClock injects a test clock.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// New constructs a Store.
func New(db *sql.DB, opts ...Option) *Store {
	s := &Store{db: db, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Folder is a read view of one tree node. ParentID 0 means root (row
// ids start at 1, so 0 is never a real parent).
type Folder struct {
	ID       int64  `json:"id"`
	ParentID int64  `json:"parent_id"`
	Name     string `json:"name"`
	System   bool   `json:"system"`
	Sort     int64  `json:"sort"`
}

// GraphMeta is a read view of one graph without its content.
type GraphMeta struct {
	ID        int64  `json:"id"`
	FolderID  int64  `json:"folder_id"`
	Name      string `json:"name"`
	Rev       int64  `json:"rev"`
	Sort      int64  `json:"sort"`
	UpdatedAt int64  `json:"updated_at"`
}

// Graph is a graph with its stored JSON content.
type Graph struct {
	GraphMeta
	JSON string
}

// cleanName trims and validates a folder/graph name.
func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrEmptyName
	}
	if len([]rune(name)) > maxNameLen {
		name = string([]rune(name)[:maxNameLen])
	}
	return name, nil
}

// Tree returns all folders and all graph metadata, each ordered by
// sort, then name, then id - ready for the editor to assemble.
func (s *Store) Tree(ctx context.Context) ([]Folder, []GraphMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, COALESCE(parent_id, 0), name, system, sort
		   FROM designer_folders
		  ORDER BY sort, name COLLATE NOCASE, id`)
	if err != nil {
		return nil, nil, fmt.Errorf("designerstore: list folders: %w", err)
	}
	defer rows.Close()
	var folders []Folder
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.ID, &f.ParentID, &f.Name, &f.System, &f.Sort); err != nil {
			return nil, nil, fmt.Errorf("designerstore: scan folder: %w", err)
		}
		folders = append(folders, f)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("designerstore: list folders: %w", err)
	}

	grows, err := s.db.QueryContext(ctx,
		`SELECT id, folder_id, name, rev, sort, updated_at
		   FROM designer_graphs
		  ORDER BY sort, name COLLATE NOCASE, id`)
	if err != nil {
		return nil, nil, fmt.Errorf("designerstore: list graphs: %w", err)
	}
	defer grows.Close()
	var graphs []GraphMeta
	for grows.Next() {
		var g GraphMeta
		if err := grows.Scan(&g.ID, &g.FolderID, &g.Name, &g.Rev, &g.Sort, &g.UpdatedAt); err != nil {
			return nil, nil, fmt.Errorf("designerstore: scan graph: %w", err)
		}
		graphs = append(graphs, g)
	}
	return folders, graphs, grows.Err()
}

// folderSystem reads a folder's system flag; ErrNotFound when the id
// has no row. Runs on any Queryer so it works inside transactions.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func folderSystem(ctx context.Context, q queryer, id int64) (bool, error) {
	var system bool
	err := q.QueryRowContext(ctx,
		`SELECT system FROM designer_folders WHERE id = ?`, id).Scan(&system)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("designerstore: read folder: %w", err)
	}
	return system, nil
}

// graphFolderSystem reads the system flag of the folder containing a
// graph; ErrNotFound when the graph id has no row.
func graphFolderSystem(ctx context.Context, q queryer, id int64) (bool, error) {
	var system bool
	err := q.QueryRowContext(ctx,
		`SELECT f.system FROM designer_graphs g
		   JOIN designer_folders f ON f.id = g.folder_id
		  WHERE g.id = ?`, id).Scan(&system)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("designerstore: read graph folder: %w", err)
	}
	return system, nil
}

// CreateFolder inserts a folder under parentID (0 = root), appended
// after its siblings. ErrNotFound when the parent does not exist,
// ErrSystemFolder when the parent is protected.
func (s *Store) CreateFolder(ctx context.Context, parentID int64, name string) (Folder, error) {
	name, err := cleanName(name)
	if err != nil {
		return Folder{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Folder{}, fmt.Errorf("designerstore: begin create folder: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if parentID != 0 {
		system, err := folderSystem(ctx, tx, parentID)
		if err != nil {
			return Folder{}, err
		}
		if system {
			return Folder{}, ErrSystemFolder
		}
	}
	var parent any
	if parentID != 0 {
		parent = parentID
	}
	var sort int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(sort)+1, 0) FROM designer_folders WHERE parent_id IS ?`,
		parent).Scan(&sort); err != nil {
		return Folder{}, fmt.Errorf("designerstore: next folder sort: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO designer_folders (parent_id, name, system, sort, updated_at)
		 VALUES (?, ?, 0, ?, ?)`,
		parent, name, sort, s.now().UnixMilli())
	if err != nil {
		return Folder{}, fmt.Errorf("designerstore: insert folder: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Folder{}, fmt.Errorf("designerstore: folder id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Folder{}, fmt.Errorf("designerstore: commit create folder: %w", err)
	}
	return Folder{ID: id, ParentID: parentID, Name: name, Sort: sort}, nil
}

// RenameFolder renames a non-system folder.
func (s *Store) RenameFolder(ctx context.Context, id int64, name string) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	system, err := folderSystem(ctx, s.db, id)
	if err != nil {
		return err
	}
	if system {
		return ErrSystemFolder
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE designer_folders SET name = ?, updated_at = ? WHERE id = ?`,
		name, s.now().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("designerstore: rename folder: %w", err)
	}
	return nil
}

// DeleteFolder deletes a non-system folder that has no subfolders and
// no graphs (ErrFolderNotEmpty otherwise).
func (s *Store) DeleteFolder(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("designerstore: begin delete folder: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	system, err := folderSystem(ctx, tx, id)
	if err != nil {
		return err
	}
	if system {
		return ErrSystemFolder
	}
	var children int
	if err := tx.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM designer_folders WHERE parent_id = ?)
		      + (SELECT COUNT(*) FROM designer_graphs WHERE folder_id = ?)`,
		id, id).Scan(&children); err != nil {
		return fmt.Errorf("designerstore: count folder children: %w", err)
	}
	if children > 0 {
		return ErrFolderNotEmpty
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM designer_folders WHERE id = ?`, id); err != nil {
		return fmt.Errorf("designerstore: delete folder: %w", err)
	}
	return tx.Commit()
}

// CreateGraph inserts an empty graph into a non-system folder,
// appended after its siblings.
func (s *Store) CreateGraph(ctx context.Context, folderID int64, name string) (GraphMeta, error) {
	name, err := cleanName(name)
	if err != nil {
		return GraphMeta{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GraphMeta{}, fmt.Errorf("designerstore: begin create graph: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	system, err := folderSystem(ctx, tx, folderID)
	if err != nil {
		return GraphMeta{}, err
	}
	if system {
		return GraphMeta{}, ErrSystemFolder
	}
	var sort int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(sort)+1, 0) FROM designer_graphs WHERE folder_id = ?`,
		folderID).Scan(&sort); err != nil {
		return GraphMeta{}, fmt.Errorf("designerstore: next graph sort: %w", err)
	}
	now := s.now().UnixMilli()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO designer_graphs (folder_id, name, graph_json, rev, sort, updated_at)
		 VALUES (?, ?, ?, 0, ?, ?)`,
		folderID, name, EmptyGraphJSON, sort, now)
	if err != nil {
		return GraphMeta{}, fmt.Errorf("designerstore: insert graph: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return GraphMeta{}, fmt.Errorf("designerstore: graph id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return GraphMeta{}, fmt.Errorf("designerstore: commit create graph: %w", err)
	}
	return GraphMeta{ID: id, FolderID: folderID, Name: name, Sort: sort, UpdatedAt: now}, nil
}

// RenameGraph renames a graph outside system folders.
func (s *Store) RenameGraph(ctx context.Context, id int64, name string) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	system, err := graphFolderSystem(ctx, s.db, id)
	if err != nil {
		return err
	}
	if system {
		return ErrSystemFolder
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE designer_graphs SET name = ?, updated_at = ? WHERE id = ?`,
		name, s.now().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("designerstore: rename graph: %w", err)
	}
	return nil
}

// DeleteGraph deletes a graph outside system folders.
func (s *Store) DeleteGraph(ctx context.Context, id int64) error {
	system, err := graphFolderSystem(ctx, s.db, id)
	if err != nil {
		return err
	}
	if system {
		return ErrSystemFolder
	}
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM designer_graphs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("designerstore: delete graph: %w", err)
	}
	return nil
}

// Graph loads one graph including its stored JSON.
func (s *Store) Graph(ctx context.Context, id int64) (Graph, error) {
	var g Graph
	err := s.db.QueryRowContext(ctx,
		`SELECT id, folder_id, name, rev, sort, updated_at, graph_json
		   FROM designer_graphs WHERE id = ?`, id).
		Scan(&g.ID, &g.FolderID, &g.Name, &g.Rev, &g.Sort, &g.UpdatedAt, &g.JSON)
	if errors.Is(err, sql.ErrNoRows) {
		return Graph{}, ErrNotFound
	}
	if err != nil {
		return Graph{}, fmt.Errorf("designerstore: load graph: %w", err)
	}
	return g, nil
}

// SaveGraph replaces a graph's content and increments its revision,
// returning the new revision. Saving is allowed in system folders too:
// the structure there is locked, the content stays editable (the later
// reader flow deep-links into the editor).
func (s *Store) SaveGraph(ctx context.Context, id int64, graphJSON string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("designerstore: begin save graph: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE designer_graphs
		    SET graph_json = ?, rev = rev + 1, updated_at = ?
		  WHERE id = ?`,
		graphJSON, s.now().UnixMilli(), id)
	if err != nil {
		return 0, fmt.Errorf("designerstore: save graph: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	var rev int64
	if err := tx.QueryRowContext(ctx,
		`SELECT rev FROM designer_graphs WHERE id = ?`, id).Scan(&rev); err != nil {
		return 0, fmt.Errorf("designerstore: read rev: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("designerstore: commit save graph: %w", err)
	}
	return rev, nil
}
