package designerstore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"carvilon.local/server/internal/db"
)

// newTestStore opens a real temp-file DB so the full migration stack
// (including the 032 seed) runs, and returns a Store plus the raw DB
// for direct seeding.
func newTestStore(t *testing.T, opts ...Option) (*Store, *db.DB) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return New(d.DB, opts...), d
}

func findFolder(fs []Folder, name string) *Folder {
	for i := range fs {
		if fs[i].Name == name {
			return &fs[i]
		}
	}
	return nil
}

func findGraph(gs []GraphMeta, name string) *GraphMeta {
	for i := range gs {
		if gs[i].Name == name {
			return &gs[i]
		}
	}
	return nil
}

// ---------- seed ----------

func TestSeed_TreeMatchesDemo(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	folders, graphs, err := s.Tree(ctx)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if len(folders) != 6 {
		t.Fatalf("seeded folders = %d, want 6", len(folders))
	}
	if len(graphs) != 6 {
		t.Fatalf("seeded graphs = %d, want 6", len(graphs))
	}

	building := findFolder(folders, "Building")
	system := findFolder(folders, "System")
	reader := findFolder(folders, "Reader")
	ground := findFolder(folders, "Ground floor")
	if building == nil || system == nil || reader == nil || ground == nil {
		t.Fatalf("seed folders missing: %+v", folders)
	}
	if building.ParentID != 0 || building.System {
		t.Errorf("Building = %+v, want root non-system", building)
	}
	if ground.ParentID != building.ID {
		t.Errorf("Ground floor parent = %d, want %d (Building)", ground.ParentID, building.ID)
	}
	if !system.System || system.ParentID != 0 {
		t.Errorf("System = %+v, want root system folder", system)
	}
	if !reader.System || reader.ParentID != system.ID {
		t.Errorf("Reader = %+v, want system folder under System", reader)
	}

	flur := findGraph(graphs, "EG · Flur")
	if flur == nil {
		t.Fatalf("seed graph 'EG · Flur' missing: %+v", graphs)
	}
	if flur.FolderID != ground.ID || flur.Rev != 0 {
		t.Errorf("EG · Flur = %+v, want in Ground floor with rev 0", flur)
	}
	// Seeded graphs are empty structure.
	g, err := s.Graph(ctx, flur.ID)
	if err != nil {
		t.Fatalf("Graph: %v", err)
	}
	if g.JSON != EmptyGraphJSON {
		t.Errorf("seed graph json = %q, want %q", g.JSON, EmptyGraphJSON)
	}
}

// ---------- folders ----------

func TestFolder_CreateRenameDelete(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	f, err := s.CreateFolder(ctx, 0, "  Keller ")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if f.Name != "Keller" {
		t.Errorf("name = %q, want trimmed %q", f.Name, "Keller")
	}
	sub, err := s.CreateFolder(ctx, f.ID, "Technik")
	if err != nil {
		t.Fatalf("CreateFolder sub: %v", err)
	}
	if sub.ParentID != f.ID {
		t.Errorf("sub parent = %d, want %d", sub.ParentID, f.ID)
	}

	if err := s.RenameFolder(ctx, f.ID, "Untergeschoss"); err != nil {
		t.Fatalf("RenameFolder: %v", err)
	}
	folders, _, err := s.Tree(ctx)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if findFolder(folders, "Untergeschoss") == nil {
		t.Error("rename not visible in tree")
	}

	// Non-empty folder refuses deletion, empty one goes.
	if err := s.DeleteFolder(ctx, f.ID); !errors.Is(err, ErrFolderNotEmpty) {
		t.Fatalf("DeleteFolder non-empty = %v, want ErrFolderNotEmpty", err)
	}
	if err := s.DeleteFolder(ctx, sub.ID); err != nil {
		t.Fatalf("DeleteFolder sub: %v", err)
	}
	if err := s.DeleteFolder(ctx, f.ID); err != nil {
		t.Fatalf("DeleteFolder emptied: %v", err)
	}
}

func TestFolder_DeleteWithGraphRefused(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	f, err := s.CreateFolder(ctx, 0, "Garage")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if _, err := s.CreateGraph(ctx, f.ID, "Tor"); err != nil {
		t.Fatalf("CreateGraph: %v", err)
	}
	if err := s.DeleteFolder(ctx, f.ID); !errors.Is(err, ErrFolderNotEmpty) {
		t.Fatalf("DeleteFolder with graph = %v, want ErrFolderNotEmpty", err)
	}
}

func TestFolder_Validation(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	if _, err := s.CreateFolder(ctx, 0, "   "); !errors.Is(err, ErrEmptyName) {
		t.Errorf("blank name = %v, want ErrEmptyName", err)
	}
	if _, err := s.CreateFolder(ctx, 99999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing parent = %v, want ErrNotFound", err)
	}
	if err := s.RenameFolder(ctx, 99999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rename missing = %v, want ErrNotFound", err)
	}
	if err := s.DeleteFolder(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete missing = %v, want ErrNotFound", err)
	}
}

// ---------- system-folder protection ----------

func TestSystemFolders_Untouchable(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	folders, _, err := s.Tree(ctx)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	for _, name := range []string{"System", "Reader"} {
		f := findFolder(folders, name)
		if f == nil {
			t.Fatalf("folder %s missing", name)
		}
		if err := s.RenameFolder(ctx, f.ID, "anders"); !errors.Is(err, ErrSystemFolder) {
			t.Errorf("rename %s = %v, want ErrSystemFolder", name, err)
		}
		if err := s.DeleteFolder(ctx, f.ID); !errors.Is(err, ErrSystemFolder) {
			t.Errorf("delete %s = %v, want ErrSystemFolder", name, err)
		}
		if _, err := s.CreateFolder(ctx, f.ID, "sub"); !errors.Is(err, ErrSystemFolder) {
			t.Errorf("create folder in %s = %v, want ErrSystemFolder", name, err)
		}
		if _, err := s.CreateGraph(ctx, f.ID, "manual"); !errors.Is(err, ErrSystemFolder) {
			t.Errorf("create graph in %s = %v, want ErrSystemFolder", name, err)
		}
	}
}

// A graph living in a system folder (created later by the reader flow,
// seeded here directly) keeps its content editable but its structure
// locked.
func TestSystemFolders_GraphContentEditableStructureLocked(t *testing.T) {
	ctx := context.Background()
	s, d := newTestStore(t)

	res, err := d.Exec(
		`INSERT INTO designer_graphs (folder_id, name, updated_at)
		 SELECT id, 'Haustuer-Reader', 1 FROM designer_folders WHERE name = 'Reader'`)
	if err != nil {
		t.Fatalf("seed reader graph: %v", err)
	}
	id, _ := res.LastInsertId()

	if err := s.RenameGraph(ctx, id, "anders"); !errors.Is(err, ErrSystemFolder) {
		t.Errorf("rename reader graph = %v, want ErrSystemFolder", err)
	}
	if err := s.DeleteGraph(ctx, id); !errors.Is(err, ErrSystemFolder) {
		t.Errorf("delete reader graph = %v, want ErrSystemFolder", err)
	}
	rev, err := s.SaveGraph(ctx, id, `{"schema":1,"nodes":[{"id":"r1"}],"edges":[]}`)
	if err != nil {
		t.Fatalf("SaveGraph in system folder: %v", err)
	}
	if rev != 1 {
		t.Errorf("rev = %d, want 1", rev)
	}
}

// ---------- graphs ----------

func TestGraph_CreateLoadSaveRev(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	folders, _, err := s.Tree(ctx)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	sandbox := findFolder(folders, "Sandbox")

	g, err := s.CreateGraph(ctx, sandbox.ID, "Experiment")
	if err != nil {
		t.Fatalf("CreateGraph: %v", err)
	}
	if g.Rev != 0 {
		t.Errorf("fresh rev = %d, want 0", g.Rev)
	}

	// rev counts up server-side on every save; content round-trips.
	body := `{"schema":1,"nodes":[{"id":"a"}],"edges":[]}`
	rev, err := s.SaveGraph(ctx, g.ID, body)
	if err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}
	if rev != 1 {
		t.Errorf("rev after first save = %d, want 1", rev)
	}
	rev, err = s.SaveGraph(ctx, g.ID, body)
	if err != nil {
		t.Fatalf("SaveGraph again: %v", err)
	}
	if rev != 2 {
		t.Errorf("rev after second save = %d, want 2", rev)
	}
	loaded, err := s.Graph(ctx, g.ID)
	if err != nil {
		t.Fatalf("Graph: %v", err)
	}
	if loaded.JSON != body || loaded.Rev != 2 {
		t.Errorf("loaded = rev %d json %q, want rev 2 json %q", loaded.Rev, loaded.JSON, body)
	}

	if err := s.RenameGraph(ctx, g.ID, "Versuch"); err != nil {
		t.Fatalf("RenameGraph: %v", err)
	}
	if err := s.DeleteGraph(ctx, g.ID); err != nil {
		t.Fatalf("DeleteGraph: %v", err)
	}
	if _, err := s.Graph(ctx, g.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Graph after delete = %v, want ErrNotFound", err)
	}
}

func TestGraph_Validation(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	if _, err := s.CreateGraph(ctx, 99999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("create in missing folder = %v, want ErrNotFound", err)
	}
	folders, _, _ := s.Tree(ctx)
	sandbox := findFolder(folders, "Sandbox")
	if _, err := s.CreateGraph(ctx, sandbox.ID, ""); !errors.Is(err, ErrEmptyName) {
		t.Errorf("blank graph name = %v, want ErrEmptyName", err)
	}
	if _, err := s.SaveGraph(ctx, 99999, "{}"); !errors.Is(err, ErrNotFound) {
		t.Errorf("save missing graph = %v, want ErrNotFound", err)
	}
	if err := s.RenameGraph(ctx, 99999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rename missing graph = %v, want ErrNotFound", err)
	}
	if err := s.DeleteGraph(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete missing graph = %v, want ErrNotFound", err)
	}
}

// New siblings land after the existing ones (sort appends).
func TestSortOrder_Appends(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	a, err := s.CreateFolder(ctx, 0, "Zubau")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	b, err := s.CreateFolder(ctx, 0, "Anbau")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if b.Sort <= a.Sort {
		t.Errorf("sibling sort not appending: %d then %d", a.Sort, b.Sort)
	}
	g1, err := s.CreateGraph(ctx, a.ID, "Zwei")
	if err != nil {
		t.Fatalf("CreateGraph: %v", err)
	}
	g2, err := s.CreateGraph(ctx, a.ID, "Eins")
	if err != nil {
		t.Fatalf("CreateGraph: %v", err)
	}
	if g2.Sort <= g1.Sort {
		t.Errorf("graph sort not appending: %d then %d", g1.Sort, g2.Sort)
	}
	// Tree keeps creation order (sort beats name).
	_, graphs, err := s.Tree(ctx)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	var inA []string
	for _, g := range graphs {
		if g.FolderID == a.ID {
			inA = append(inA, g.Name)
		}
	}
	if len(inA) != 2 || inA[0] != "Zwei" || inA[1] != "Eins" {
		t.Errorf("graph order in folder = %v, want [Zwei Eins]", inA)
	}
}
