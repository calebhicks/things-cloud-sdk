package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	thingscloud "github.com/arthursoares/things-cloud-sdk"
)

// setHermeticEnv pins fake credentials and a throwaway cache path for the
// duration of a test. t.Setenv overrides and later restores THINGS_USERNAME,
// THINGS_PASSWORD, and the endpoint/cache knobs, so ambient real credentials
// or a real cache can never leak into these tests and every request stays on
// the local httptest server.
func setHermeticEnv(t *testing.T) {
	t.Helper()
	t.Setenv("THINGS_USERNAME", "test@example.com")
	t.Setenv("THINGS_PASSWORD", "secret")
	t.Setenv("THINGS_ENDPOINT", "")
	t.Setenv("THINGS_DEBUG", "")
	t.Setenv("THINGS_MCP_CACHE", filepath.Join(t.TempDir(), "mcp-state.json"))
}

func TestEnsureCloudDoesNotDuplicateVerify(t *testing.T) {
	var verifies int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version/1/account/test@example.com":
			verifies++
			fmt.Fprint(w, `{"email":"test@example.com","history-key":"history-id","status":"SYAccountStatusActive"}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	setHermeticEnv(t)
	server := &mcpServer{endpoint: ts.URL}
	if err := server.ensureCloud(); err != nil {
		t.Fatalf("ensureCloud failed: %v", err)
	}
	if verifies != 1 {
		t.Fatalf("verify requests = %d, want 1", verifies)
	}
	if server.history == nil || server.history.ID != "history-id" {
		t.Fatalf("history = %#v, want history-id", server.history)
	}
}

func TestListTasksUsesStateCacheAcrossCalls(t *testing.T) {
	var itemsRequests int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version/1/account/test@example.com":
			fmt.Fprint(w, `{"email":"test@example.com","history-key":"history-id","status":"SYAccountStatusActive"}`)
		case "/version/1/history/history-id":
			fmt.Fprint(w, `{"latest-server-index":1,"latest-schema-version":301}`)
		case "/version/1/history/history-id/items":
			itemsRequests++
			if got := r.URL.Query().Get("start-index"); got != "0" {
				t.Errorf("start-index = %s, want 0", got)
			}
			fmt.Fprint(w, `{"items":[{"task-1":{"e":"Task6","t":0,"p":{"tt":"Alpha","tp":0,"st":1,"ss":0}}}],"current-item-index":1,"schema":301}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	setHermeticEnv(t)
	server := &mcpServer{endpoint: ts.URL}

	assertAlpha := func(result toolResult, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("listTasks failed: %v", err)
		}
		if result.IsError {
			t.Fatalf("listTasks returned tool error: %#v", result)
		}
		var tasks []struct {
			UUID  string `json:"uuid"`
			Title string `json:"title"`
		}
		if err := json.Unmarshal([]byte(result.Content[0].Text), &tasks); err != nil {
			t.Fatalf("unmarshal tasks: %v", err)
		}
		if len(tasks) != 1 || tasks[0].UUID != "task-1" || tasks[0].Title != "Alpha" {
			t.Fatalf("tasks = %#v, want task-1 Alpha", tasks)
		}
	}

	assertAlpha(server.listTasks("all", "", 0))
	if itemsRequests != 1 {
		t.Fatalf("items requests after first read = %d, want 1", itemsRequests)
	}

	// The cursor is cached at the server head, so repeated reads must not
	// replay history.
	assertAlpha(server.listTasks("all", "", 0))
	if itemsRequests != 1 {
		t.Fatalf("items requests after second read = %d, want 1 (no replay)", itemsRequests)
	}

	// An empty view still serializes as a JSON array.
	result, err := server.listTasks("today", "", 0)
	if err != nil {
		t.Fatalf("listTasks today failed: %v", err)
	}
	if result.Content[0].Text != "[]" {
		t.Fatalf("empty view serialized as %q, want []", result.Content[0].Text)
	}
}

func TestToolJSONEmptySliceSerializesAsArray(t *testing.T) {
	// Every MCP list result is documented as a JSON array. The list functions
	// must initialize empty slices so empty results serialize as [] rather
	// than null (a nil slice marshals to null).
	for name, v := range map[string]any{
		"tasks":    []simpleTask{},
		"projects": []simpleProject{},
		"areas":    []simpleArea{},
		"tags":     []simpleTag{},
	} {
		if got := toolJSON(v).Content[0].Text; got != "[]" {
			t.Fatalf("empty %s list serialized as %q, want []", name, got)
		}
	}
}

func TestHandleInitialize(t *testing.T) {
	server := &mcpServer{}
	resp, ok := server.handle(rpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "initialize",
	})
	if !ok {
		t.Fatal("initialize did not produce a response")
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result = %T, want map", resp.Result)
	}
	if result["protocolVersion"] != protocolVersion {
		t.Fatalf("protocolVersion = %v, want %s", result["protocolVersion"], protocolVersion)
	}
	serverInfo, ok := result["serverInfo"].(map[string]string)
	if !ok {
		t.Fatalf("serverInfo = %T, want map[string]string", result["serverInfo"])
	}
	if serverInfo["version"] != serverVersion {
		t.Fatalf("serverInfo.version = %v, want %s", serverInfo["version"], serverVersion)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %#v", resp.Error)
	}
}

func TestToolsListIncludesCoreTools(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range tools() {
		names[tool.Name] = true
		if tool.InputSchema["type"] != "object" {
			t.Fatalf("%s input schema type = %v, want object", tool.Name, tool.InputSchema["type"])
		}
	}
	for _, name := range []string{
		"list_tasks",
		"search_tasks",
		"create_task",
		"complete_task",
		"edit_task",
		"batch_tasks",
		"trash_task",
		"move_task_to_today",
		"add_checklist",
		"list_projects",
		"list_areas",
		"list_tags",
	} {
		if !names[name] {
			t.Fatalf("missing tool %s", name)
		}
	}
}

func TestCreateTaskDryRunUsesCanonicalUUID(t *testing.T) {
	server := &mcpServer{}
	result, err := server.createTask("Dry run task", "note", "today", true)
	if err != nil {
		t.Fatalf("createTask dry-run failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("dry-run returned tool error: %#v", result)
	}

	content := result.Content[0].Text
	var payload struct {
		Status string `json:"status"`
		UUID   string `json:"uuid"`
		Item   struct {
			E string `json:"e"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		t.Fatalf("unmarshal dry-run content failed: %v", err)
	}
	if payload.Status != "dry-run" {
		t.Fatalf("status = %q, want dry-run", payload.Status)
	}
	// Generated identifiers must be the core's canonical Base58 form; a
	// non-canonical identifier poisons the sync history.
	if err := thingscloud.ValidateUUID(payload.UUID); err != nil {
		t.Fatalf("dry-run uuid %q is not canonical: %v", payload.UUID, err)
	}
	if payload.Item.E != "Task6" {
		t.Fatalf("item kind = %q, want Task6", payload.Item.E)
	}
}

func TestCompleteTaskDryRunDoesNotRequireCloud(t *testing.T) {
	server := &mcpServer{}
	taskUUID := thingscloud.NewUUID()
	result, err := server.completeTask(taskUUID, true)
	if err != nil {
		t.Fatalf("completeTask dry-run failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("dry-run returned tool error: %#v", result)
	}
	var payload struct {
		Status string `json:"status"`
		UUID   string `json:"uuid"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
		t.Fatalf("unmarshal dry-run content failed: %v", err)
	}
	if payload.Status != "dry-run" || payload.UUID != taskUUID {
		t.Fatalf("payload = %#v, want dry-run for %s", payload, taskUUID)
	}
}

func TestEditTaskDryRunDoesNotRequireCloud(t *testing.T) {
	server := &mcpServer{}
	taskUUID := thingscloud.NewUUID()
	result, err := server.editTask(taskUUID, "New title", "new note", "anytime", true)
	if err != nil {
		t.Fatalf("editTask dry-run failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("dry-run returned tool error: %#v", result)
	}
	var payload struct {
		Status string `json:"status"`
		UUID   string `json:"uuid"`
		Item   struct {
			E string `json:"e"`
			P struct {
				Title string `json:"tt"`
				St    int    `json:"st"`
			} `json:"p"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
		t.Fatalf("unmarshal dry-run content failed: %v", err)
	}
	if payload.Status != "dry-run" || payload.UUID != taskUUID {
		t.Fatalf("payload = %#v, want dry-run for %s", payload, taskUUID)
	}
	if payload.Item.E != "Task6" || payload.Item.P.Title != "New title" || payload.Item.P.St != 1 {
		t.Fatalf("item payload = %#v, want Task6 title and anytime schedule", payload.Item)
	}
}

func TestWriteToolsRejectNonCanonicalUUID(t *testing.T) {
	server := &mcpServer{}
	// Even dry-run must refuse a non-canonical identifier: previewing an
	// envelope that can never be written safely is misleading.
	if _, err := server.completeTask("task-1", true); err == nil {
		t.Fatal("complete with non-canonical uuid should be rejected")
	}
	if _, err := server.editTask("not-a-uuid", "T", "", "", true); err == nil {
		t.Fatal("edit with non-canonical uuid should be rejected")
	}
	if _, err := server.trashTask("zzzzzzzzzzzzzzzzzzzzzz", true); err == nil {
		t.Fatal("trash with over-range uuid should be rejected")
	}
	if _, err := server.addChecklist("task-1", []string{"One"}, true); err == nil {
		t.Fatal("add_checklist with non-canonical uuid should be rejected")
	}
	if _, err := server.batchTasks([]batchTaskOp{{Cmd: "complete", UUID: "task-1"}}, true); err == nil {
		t.Fatal("batch complete with non-canonical uuid should be rejected")
	}
	if _, err := server.batchTasks([]batchTaskOp{{Cmd: "create", Title: "X", UUID: "task-1"}}, true); err == nil {
		t.Fatal("batch create with non-canonical caller uuid should be rejected")
	}
}

func TestBatchTasksDryRunDoesNotRequireCloud(t *testing.T) {
	server := &mcpServer{}
	completeUUID := thingscloud.NewUUID()
	result, err := server.batchTasks([]batchTaskOp{
		{Cmd: "create", Title: "Task one", When: "today"},
		{Cmd: "complete", UUID: completeUUID},
	}, true)
	if err != nil {
		t.Fatalf("batchTasks dry-run failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("dry-run returned tool error: %#v", result)
	}
	var payload struct {
		Status     string `json:"status"`
		Operations int    `json:"operations"`
		Items      []struct {
			T int    `json:"t"`
			E string `json:"e"`
		} `json:"items"`
		Results []map[string]string `json:"results"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
		t.Fatalf("unmarshal dry-run content failed: %v", err)
	}
	if payload.Status != "dry-run" || payload.Operations != 2 {
		t.Fatalf("payload = %#v, want dry-run with 2 operations", payload)
	}
	if len(payload.Items) != 2 || payload.Items[0].E != "Task6" || payload.Items[0].T != 0 || payload.Items[1].T != 1 {
		t.Fatalf("items = %#v, want a Task6 create and an update", payload.Items)
	}
	if len(payload.Results) != 2 || payload.Results[1]["uuid"] != completeUUID {
		t.Fatalf("results = %#v, want complete uuid %s", payload.Results, completeUUID)
	}
	if err := thingscloud.ValidateUUID(payload.Results[0]["uuid"]); err != nil {
		t.Fatalf("generated create uuid %q is not canonical: %v", payload.Results[0]["uuid"], err)
	}
}

func TestBatchCreatesGetDistinctPositiveIx(t *testing.T) {
	server := &mcpServer{}
	result, err := server.batchTasks([]batchTaskOp{
		{Cmd: "create", Title: "One"},
		{Cmd: "complete", UUID: thingscloud.NewUUID()},
		{Cmd: "create", Title: "Two"},
		{Cmd: "create", Title: "Three"},
	}, true)
	if err != nil {
		t.Fatalf("batchTasks dry-run failed: %v", err)
	}
	var payload struct {
		Items []struct {
			T int `json:"t"`
			P struct {
				Ix *int `json:"ix"`
			} `json:"p"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
		t.Fatalf("unmarshal dry-run content failed: %v", err)
	}
	if len(payload.Items) != 4 {
		t.Fatalf("items = %d, want 4", len(payload.Items))
	}
	// Both poisoned batches from the 2026-08-12 incident sent ix:0 for every
	// create; ix must be distinct and positive per created item.
	wantIx := map[int]int{0: 1, 2: 2, 3: 3}
	seen := map[int]bool{}
	for i, item := range payload.Items {
		want, isCreate := wantIx[i]
		if !isCreate {
			continue
		}
		if item.T != 0 {
			t.Fatalf("item %d action = %d, want create", i, item.T)
		}
		if item.P.Ix == nil || *item.P.Ix != want {
			t.Fatalf("item %d ix = %v, want %d", i, item.P.Ix, want)
		}
		if *item.P.Ix <= 0 || seen[*item.P.Ix] {
			t.Fatalf("item %d ix = %d, want distinct positive", i, *item.P.Ix)
		}
		seen[*item.P.Ix] = true
	}
}

func TestBatchTasksCommitsSequentially(t *testing.T) {
	var commits int
	trashUUID := thingscloud.NewUUID()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version/1/history/history-id/items":
			fmt.Fprint(w, `{"items":[],"current-item-index":3,"schema":301}`)
		case "/version/1/history/history-id/commit":
			commits++
			// First commit builds on the synced head (3); each subsequent
			// commit chains on the head returned by the previous response.
			want := map[int]string{1: "3", 2: "5"}[commits]
			if got := r.URL.Query().Get("ancestor-index"); got != want {
				t.Errorf("commit %d ancestor-index = %s, want %s", commits, got, want)
			}
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode commit body: %v", err)
			}
			if len(body) != 1 {
				t.Errorf("commit body has %d entries, want 1 (sequential single-item commits)", len(body))
			}
			fmt.Fprint(w, `{"server-head-index":5}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client := thingscloud.New(ts.URL, "test@example.com", "secret")
	server := &mcpServer{client: client, history: client.HistoryWithID("history-id")}

	result, err := server.batchTasks([]batchTaskOp{
		{Cmd: "create", Title: "Task one", When: "anytime"},
		{Cmd: "trash", UUID: trashUUID},
	}, false)
	if err != nil {
		t.Fatalf("batchTasks failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("batchTasks returned tool error: %#v", result)
	}
	if commits != 2 {
		t.Fatalf("commits = %d, want 2 (one commit per item)", commits)
	}
	var payload struct {
		Status     string              `json:"status"`
		Operations int                 `json:"operations"`
		Results    []map[string]string `json:"results"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
		t.Fatalf("unmarshal content failed: %v", err)
	}
	if payload.Status != "ok" || payload.Operations != 2 || len(payload.Results) != 2 {
		t.Fatalf("payload = %#v, want ok with 2 operations", payload)
	}
}

func TestBatchTasksRejectsUnsupportedCmdAndUnknownFields(t *testing.T) {
	server := &mcpServer{}
	if _, err := server.batchTasks([]batchTaskOp{{Cmd: "purge", UUID: thingscloud.NewUUID()}}, true); err == nil {
		t.Fatal("purge cmd should be rejected")
	}
	if _, err := server.batchTasks(nil, true); err == nil {
		t.Fatal("empty operations should be rejected")
	}
	dup := thingscloud.NewUUID()
	if _, err := server.batchTasks([]batchTaskOp{
		{Cmd: "complete", UUID: dup},
		{Cmd: "trash", UUID: dup},
	}, true); err == nil {
		t.Fatal("duplicate uuids in one batch should be rejected")
	}

	// Unsupported operation fields must fail loudly, not silently drop.
	_, err := server.callTool(json.RawMessage(`{"name":"batch_tasks","arguments":{"operations":[{"cmd":"create","title":"X","project":"p-1"}],"dry_run":true}}`))
	if err == nil {
		t.Fatal("unknown operation field should be rejected")
	}
}

func TestAddChecklistDryRunDoesNotRequireCloud(t *testing.T) {
	server := &mcpServer{}
	taskUUID := thingscloud.NewUUID()
	result, err := server.addChecklist(taskUUID, []string{"One", "Two"}, true)
	if err != nil {
		t.Fatalf("addChecklist dry-run failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("dry-run returned tool error: %#v", result)
	}
	var payload struct {
		Status string `json:"status"`
		UUID   string `json:"uuid"`
		Items  []struct {
			E string `json:"e"`
			P struct {
				Ix int `json:"ix"`
			} `json:"p"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
		t.Fatalf("unmarshal dry-run content failed: %v", err)
	}
	if payload.Status != "dry-run" || payload.UUID != taskUUID || len(payload.Items) != 2 {
		t.Fatalf("payload = %#v, want two dry-run checklist items", payload)
	}
	if payload.Items[0].E != "ChecklistItem3" {
		t.Fatalf("item kind = %q, want ChecklistItem3", payload.Items[0].E)
	}
	if payload.Items[0].P.Ix != 1 || payload.Items[1].P.Ix != 2 {
		t.Fatalf("checklist ix = %d,%d, want distinct positive 1,2", payload.Items[0].P.Ix, payload.Items[1].P.Ix)
	}
}
