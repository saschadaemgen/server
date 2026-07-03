package httpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// ---------- helpers ----------

type designerTreeResp struct {
	Folders []struct {
		ID       int64  `json:"id"`
		ParentID int64  `json:"parent_id"`
		Name     string `json:"name"`
		System   bool   `json:"system"`
	} `json:"folders"`
	Graphs []struct {
		ID       int64  `json:"id"`
		FolderID int64  `json:"folder_id"`
		Name     string `json:"name"`
		Rev      int64  `json:"rev"`
	} `json:"graphs"`
}

func designerGetTree(t *testing.T, env *testEnv) designerTreeResp {
	t.Helper()
	resp, err := env.client.Get(env.ts.URL + "/a/designer/tree")
	if err != nil {
		t.Fatalf("GET tree: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET tree = %d, want 200", resp.StatusCode)
	}
	var tree designerTreeResp
	if err := json.NewDecoder(resp.Body).Decode(&tree); err != nil {
		t.Fatalf("decode tree: %v", err)
	}
	return tree
}

func designerPost(t *testing.T, env *testEnv, path, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := env.client.Post(env.ts.URL+path, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	var data map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&data)
	return resp, data
}

func (tr designerTreeResp) folderID(t *testing.T, name string) int64 {
	t.Helper()
	for _, f := range tr.Folders {
		if f.Name == name {
			return f.ID
		}
	}
	t.Fatalf("folder %q not in tree", name)
	return 0
}

// ---------- auth ----------

func TestDesignerTree_RequiresSession(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/a/designer/tree")
	if err != nil {
		t.Fatalf("GET tree: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauthenticated GET tree = %d, want 303", resp.StatusCode)
	}
}

func TestDesignerGraphSave_RequiresSession(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Post(env.ts.URL+"/a/designer/graphs/1/save",
		"application/json", strings.NewReader(`{"graph":{}}`))
	if err != nil {
		t.Fatalf("POST save: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauthenticated save = %d, want 303", resp.StatusCode)
	}
}

// ---------- tree ----------

func TestDesignerTree_SeededStructure(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	tree := designerGetTree(t, env)
	if len(tree.Folders) != 6 || len(tree.Graphs) != 6 {
		t.Fatalf("seed tree = %d folders / %d graphs, want 6/6", len(tree.Folders), len(tree.Graphs))
	}
	var system, reader bool
	for _, f := range tree.Folders {
		if f.Name == "System" && f.System && f.ParentID == 0 {
			system = true
		}
		if f.Name == "Reader" && f.System {
			reader = true
		}
	}
	if !system || !reader {
		t.Errorf("system folders missing/unflagged in tree: %+v", tree.Folders)
	}
}

// ---------- folder CRUD ----------

func TestDesignerFolders_CreateRenameDelete(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tree := designerGetTree(t, env)
	building := tree.folderID(t, "Building")

	resp, data := designerPost(t, env, "/a/designer/folders",
		fmt.Sprintf(`{"parent_id":%d,"name":"Keller"}`, building))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create folder = %d, want 200 (%v)", resp.StatusCode, data)
	}
	folder, _ := data["folder"].(map[string]any)
	id := int64(folder["id"].(float64))

	resp, _ = designerPost(t, env, fmt.Sprintf("/a/designer/folders/%d/rename", id), `{"name":"Untergeschoss"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename folder = %d, want 200", resp.StatusCode)
	}
	tree = designerGetTree(t, env)
	found := false
	for _, f := range tree.Folders {
		if f.ID == id && f.Name == "Untergeschoss" && f.ParentID == building {
			found = true
		}
	}
	if !found {
		t.Errorf("renamed folder not in tree: %+v", tree.Folders)
	}

	resp, _ = designerPost(t, env, fmt.Sprintf("/a/designer/folders/%d/delete", id), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete folder = %d, want 200", resp.StatusCode)
	}

	// Blank name and missing parent are rejected.
	resp, _ = designerPost(t, env, "/a/designer/folders", `{"name":"  "}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("blank folder name = %d, want 400", resp.StatusCode)
	}
	resp, _ = designerPost(t, env, "/a/designer/folders", `{"parent_id":99999,"name":"x"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing parent = %d, want 404", resp.StatusCode)
	}
}

func TestDesignerFolders_DeleteNonEmptyConflicts(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tree := designerGetTree(t, env)

	// Building contains Ground/First floor -> 409.
	resp, _ := designerPost(t, env,
		fmt.Sprintf("/a/designer/folders/%d/delete", tree.folderID(t, "Building")), "")
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("delete non-empty folder = %d, want 409", resp.StatusCode)
	}
	// Ground floor contains graphs -> 409.
	resp, _ = designerPost(t, env,
		fmt.Sprintf("/a/designer/folders/%d/delete", tree.folderID(t, "Ground floor")), "")
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("delete folder with graphs = %d, want 409", resp.StatusCode)
	}
}

// ---------- system-folder protection over HTTP ----------

func TestDesignerSystemFolders_MutationsRejected(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tree := designerGetTree(t, env)

	for _, name := range []string{"System", "Reader"} {
		id := tree.folderID(t, name)
		if resp, _ := designerPost(t, env, fmt.Sprintf("/a/designer/folders/%d/rename", id), `{"name":"x"}`); resp.StatusCode != http.StatusForbidden {
			t.Errorf("rename %s = %d, want 403", name, resp.StatusCode)
		}
		if resp, _ := designerPost(t, env, fmt.Sprintf("/a/designer/folders/%d/delete", id), ""); resp.StatusCode != http.StatusForbidden {
			t.Errorf("delete %s = %d, want 403", name, resp.StatusCode)
		}
		if resp, _ := designerPost(t, env, "/a/designer/folders", fmt.Sprintf(`{"parent_id":%d,"name":"sub"}`, id)); resp.StatusCode != http.StatusForbidden {
			t.Errorf("create folder in %s = %d, want 403", name, resp.StatusCode)
		}
		if resp, _ := designerPost(t, env, "/a/designer/graphs", fmt.Sprintf(`{"folder_id":%d,"name":"manual"}`, id)); resp.StatusCode != http.StatusForbidden {
			t.Errorf("create graph in %s = %d, want 403", name, resp.StatusCode)
		}
	}
}

// ---------- graph CRUD + save/rev ----------

func TestDesignerGraphs_CreateSaveLoadRoundtrip(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tree := designerGetTree(t, env)
	sandbox := tree.folderID(t, "Sandbox")

	resp, data := designerPost(t, env, "/a/designer/graphs",
		fmt.Sprintf(`{"folder_id":%d,"name":"Experiment"}`, sandbox))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create graph = %d (%v), want 200", resp.StatusCode, data)
	}
	graph, _ := data["graph"].(map[string]any)
	id := int64(graph["id"].(float64))

	// Save twice: rev counts up server-side.
	body := `{"graph":{"schema":1,"nodes":[{"id":"a","ui":{"x":1,"y":2}}],"edges":[]}}`
	resp, data = designerPost(t, env, fmt.Sprintf("/a/designer/graphs/%d/save", id), body)
	if resp.StatusCode != http.StatusOK || data["rev"].(float64) != 1 {
		t.Fatalf("first save = %d rev %v, want 200 rev 1", resp.StatusCode, data["rev"])
	}
	resp, data = designerPost(t, env, fmt.Sprintf("/a/designer/graphs/%d/save", id), body)
	if resp.StatusCode != http.StatusOK || data["rev"].(float64) != 2 {
		t.Fatalf("second save = %d rev %v, want 200 rev 2", resp.StatusCode, data["rev"])
	}

	// Load returns the stored content verbatim plus meta.
	gresp, err := env.client.Get(env.ts.URL + fmt.Sprintf("/a/designer/graphs/%d", id))
	if err != nil {
		t.Fatalf("GET graph: %v", err)
	}
	defer gresp.Body.Close()
	if gresp.StatusCode != http.StatusOK {
		t.Fatalf("GET graph = %d, want 200", gresp.StatusCode)
	}
	var loaded struct {
		ID    int64           `json:"id"`
		Name  string          `json:"name"`
		Rev   int64           `json:"rev"`
		Graph json.RawMessage `json:"graph"`
	}
	if err := json.NewDecoder(gresp.Body).Decode(&loaded); err != nil {
		t.Fatalf("decode graph: %v", err)
	}
	if loaded.Rev != 2 || loaded.Name != "Experiment" {
		t.Errorf("loaded meta = %+v, want rev 2 name Experiment", loaded)
	}
	if !strings.Contains(string(loaded.Graph), `"id":"a"`) {
		t.Errorf("loaded graph json = %s, want saved nodes", loaded.Graph)
	}

	// Rename + delete.
	if resp, _ := designerPost(t, env, fmt.Sprintf("/a/designer/graphs/%d/rename", id), `{"name":"Versuch"}`); resp.StatusCode != http.StatusOK {
		t.Errorf("rename graph = %d, want 200", resp.StatusCode)
	}
	if resp, _ := designerPost(t, env, fmt.Sprintf("/a/designer/graphs/%d/delete", id), ""); resp.StatusCode != http.StatusOK {
		t.Errorf("delete graph = %d, want 200", resp.StatusCode)
	}
	gone, err := env.client.Get(env.ts.URL + fmt.Sprintf("/a/designer/graphs/%d", id))
	if err != nil {
		t.Fatalf("GET deleted graph: %v", err)
	}
	gone.Body.Close()
	if gone.StatusCode != http.StatusNotFound {
		t.Errorf("GET deleted graph = %d, want 404", gone.StatusCode)
	}
}

func TestDesignerGraphSave_Validation(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tree := designerGetTree(t, env)
	gid := tree.Graphs[0].ID

	// Graph must be a JSON object.
	if resp, _ := designerPost(t, env, fmt.Sprintf("/a/designer/graphs/%d/save", gid), `{"graph":[1,2]}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("array graph = %d, want 400", resp.StatusCode)
	}
	if resp, _ := designerPost(t, env, fmt.Sprintf("/a/designer/graphs/%d/save", gid), `not json`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("junk body = %d, want 400", resp.StatusCode)
	}
	// Unknown ids are 404, junk ids 400.
	if resp, _ := designerPost(t, env, "/a/designer/graphs/99999/save", `{"graph":{}}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("save unknown graph = %d, want 404", resp.StatusCode)
	}
	if resp, _ := designerPost(t, env, "/a/designer/graphs/abc/save", `{"graph":{}}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("save junk id = %d, want 400", resp.StatusCode)
	}
}

// The seeded demo graphs load as empty structures (rev 0), so the
// editor's first autosave adopts the current canvas without a reload
// ever fabricating content.
func TestDesignerGraphs_SeededEmpty(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tree := designerGetTree(t, env)

	resp, err := env.client.Get(env.ts.URL + fmt.Sprintf("/a/designer/graphs/%d", tree.Graphs[0].ID))
	if err != nil {
		t.Fatalf("GET graph: %v", err)
	}
	defer resp.Body.Close()
	var loaded struct {
		Rev   int64 `json:"rev"`
		Graph struct {
			Nodes []any `json:"nodes"`
			Edges []any `json:"edges"`
		} `json:"graph"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&loaded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if loaded.Rev != 0 || len(loaded.Graph.Nodes) != 0 || len(loaded.Graph.Edges) != 0 {
		t.Errorf("seed graph = rev %d, %d nodes, %d edges; want empty rev 0",
			loaded.Rev, len(loaded.Graph.Nodes), len(loaded.Graph.Edges))
	}
}

// The host page forwards a numeric ?g deep link onto the iframe src and
// drops anything else.
func TestDesignerHostPage_ForwardsGraphDeepLink(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	resp, err := env.client.Get(env.ts.URL + "/a/designer?g=42")
	if err != nil {
		t.Fatalf("GET /a/designer?g=42: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `src="/a/designer/?g=42"`) {
		t.Errorf("iframe src missing deep link, body has %v", strings.Contains(body, "designer-frame"))
	}

	resp, err = env.client.Get(env.ts.URL + "/a/designer?g=<script>")
	if err != nil {
		t.Fatalf("GET /a/designer?g=<script>: %v", err)
	}
	body = readBody(t, resp)
	if !strings.Contains(body, `src="/a/designer/"`) {
		t.Errorf("non-numeric deep link not dropped")
	}
}
