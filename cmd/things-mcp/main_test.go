package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

	assertAlpha(server.listTasks("all", "", "", "", 0))
	if itemsRequests != 1 {
		t.Fatalf("items requests after first read = %d, want 1", itemsRequests)
	}

	// The cursor is cached at the server head, so repeated reads must not
	// replay history.
	assertAlpha(server.listTasks("all", "", "", "", 0))
	if itemsRequests != 1 {
		t.Fatalf("items requests after second read = %d, want 1 (no replay)", itemsRequests)
	}

	// An empty view still serializes as a JSON array.
	result, err := server.listTasks("today", "", "", "", 0)
	if err != nil {
		t.Fatalf("listTasks today failed: %v", err)
	}
	if result.Content[0].Text != "[]" {
		t.Fatalf("empty view serialized as %q, want []", result.Content[0].Text)
	}
}

func TestDescriptionsCarryMethodologySemantics(t *testing.T) {
	// The tool surface teaches Things' own methodology as facts about the
	// app, so any LLM host uses the tools the way Things means them.
	server := &mcpServer{}
	resp, ok := server.handle(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "initialize"})
	if !ok {
		t.Fatal("initialize did not produce a response")
	}
	instructions := resp.Result.(map[string]any)["instructions"].(string)
	for _, want := range []string{"inbox", "finishable outcomes", "ongoing responsibilities", "real external due date", "curated", "dry_run"} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("instructions missing %q: %s", want, instructions)
		}
	}

	byName := map[string]toolDefinition{}
	for _, tool := range tools() {
		byName[tool.Name] = tool
	}
	assertContains := func(name, where, text string, wants ...string) {
		t.Helper()
		for _, want := range wants {
			if !strings.Contains(strings.ToLower(text), strings.ToLower(want)) {
				t.Fatalf("%s %s missing %q: %s", name, where, want, text)
			}
		}
	}
	propDesc := func(name, prop string) string {
		t.Helper()
		props := byName[name].InputSchema["properties"].(map[string]any)
		p, ok := props[prop].(map[string]any)
		if !ok {
			t.Fatalf("%s has no %s property", name, prop)
		}
		return p["description"].(string)
	}

	// when: capture/commitment semantics and the Today rule.
	assertContains("create_task", "when", propDesc("create_task", "when"),
		"not yet triaged", "committed and actionable", "deliberately not committed", "commitment boundary", "curated", "leave Today curation to the human")
	// deadline: real external dates only, never priority.
	assertContains("edit_task", "deadline", propDesc("edit_task", "deadline"),
		"external due dates", "never a priority flag", "plan the work")
	// scheduled: hibernation semantics.
	assertContains("batch_tasks", "scheduled", propDesc("create_task", "scheduled"),
		"hibernates", "surfaces automatically", "before it")
	// project = finishable outcome, not an ongoing responsibility.
	assertContains("create_project", "description", byName["create_project"].Description,
		"outcome", "never be checked off", "area")
	// tags: small cross-cutting filter set.
	assertContains("create_tag", "description", byName["create_tag"].Description,
		"cross-cutting", "recurring basis")
	assertContains("edit_task", "tags", propDesc("edit_task", "tags"),
		"replaces the task's whole tag set", "cross-cutting")
	// checklist: sub-steps of one to-do.
	assertContains("add_checklist", "description", byName["add_checklist"].Description,
		"sub-steps", "project")
	// Today curation on the today-mover.
	assertContains("move_task_to_today", "description", byName["move_task_to_today"].Description,
		"curated", "explicit request")
}

func TestGetTaskAndContainerFilters(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version/1/account/test@example.com":
			fmt.Fprint(w, `{"email":"test@example.com","history-key":"history-id","status":"SYAccountStatusActive"}`)
		case "/version/1/history/history-id":
			fmt.Fprint(w, `{"latest-server-index":4,"latest-schema-version":301}`)
		case "/version/1/history/history-id/items":
			fmt.Fprint(w, `{"items":[`+
				`{"area-1":{"e":"Area3","t":0,"p":{"tt":"Home"}}},`+
				`{"proj-1":{"e":"Task6","t":0,"p":{"tt":"Renovation","tp":1,"st":1,"ss":0}}},`+
				`{"task-a":{"e":"Task6","t":0,"p":{"tt":"Paint wall","tp":0,"st":1,"ss":0,"pr":["proj-1"],"nt":{"_t":"tx","ch":0,"v":"two coats","t":1}}}},`+
				`{"task-b":{"e":"Task6","t":0,"p":{"tt":"Water plants","tp":0,"st":1,"ss":0,"ar":["area-1"]}}}`+
				`],"current-item-index":4,"schema":301}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	setHermeticEnv(t)
	server := &mcpServer{endpoint: ts.URL}

	// get_task with a UUID prefix, like the CLI show command.
	result, err := server.getTask("task-a")
	if err != nil {
		t.Fatalf("getTask failed: %v", err)
	}
	var task fullTask
	if err := json.Unmarshal([]byte(result.Content[0].Text), &task); err != nil {
		t.Fatalf("unmarshal task: %v", err)
	}
	if task.UUID != "task-a" || task.Title != "Paint wall" || task.Note != "two coats" {
		t.Fatalf("task = %+v", task)
	}
	if len(task.ParentIDs) != 1 || task.ParentIDs[0] != "proj-1" || task.IsProject {
		t.Fatalf("task containers = %+v", task)
	}
	if r, err := server.getTask("nope"); err != nil || !r.IsError {
		t.Fatalf("missing task should return a tool error, got %v/%v", r, err)
	}

	// list_tasks --project filter by case-insensitive title.
	result, err = server.listTasks("all", "", "", "renovation", 0)
	if err != nil {
		t.Fatalf("listTasks project filter failed: %v", err)
	}
	var tasks []struct {
		UUID string `json:"uuid"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &tasks); err != nil {
		t.Fatalf("unmarshal tasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].UUID != "task-a" {
		t.Fatalf("project filter tasks = %#v, want task-a only", tasks)
	}

	// list_tasks --area filter.
	result, err = server.listTasks("all", "", "home", "", 0)
	if err != nil {
		t.Fatalf("listTasks area filter failed: %v", err)
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &tasks); err != nil {
		t.Fatalf("unmarshal tasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].UUID != "task-b" {
		t.Fatalf("area filter tasks = %#v, want task-b only", tasks)
	}

	// Unknown container names fail like the CLI, instead of silently
	// returning everything.
	if r, err := server.listTasks("all", "", "garage", "", 0); err != nil || !r.IsError {
		t.Fatalf("unknown area should return a tool error, got %v/%v", r, err)
	}
	if r, err := server.listTasks("all", "", "", "moon base", 0); err != nil || !r.IsError {
		t.Fatalf("unknown project should return a tool error, got %v/%v", r, err)
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
		"get_task",
		"search_tasks",
		"create_task",
		"create_project",
		"create_heading",
		"create_area",
		"create_tag",
		"complete_task",
		"edit_task",
		"batch_tasks",
		"trash_task",
		"purge_task",
		"move_task",
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
	result, err := server.createTask(createTaskArgs{Title: "Dry run task", Note: "note", When: "today", DryRun: true})
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

func TestCreateTaskFullOptionsFollowCLIConventions(t *testing.T) {
	server := &mcpServer{}
	projectUUID := thingscloud.NewUUID()
	tagUUID := thingscloud.NewUUID()
	callerUUID := thingscloud.NewUUID()

	result, err := server.createTask(createTaskArgs{
		Title:     "Full task",
		Note:      "n",
		Scheduled: "2026-09-01",
		Deadline:  "2026-09-15",
		Project:   projectUUID,
		Tags:      []string{tagUUID},
		Checklist: []string{"Step one", "", "Step two"},
		UUID:      callerUUID,
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("createTask failed: %v", err)
	}
	var payload struct {
		Status string `json:"status"`
		UUID   string `json:"uuid"`
		Item   struct {
			P struct {
				Pr []string `json:"pr"`
				Tg []string `json:"tg"`
				St int      `json:"st"`
				Sr *int64   `json:"sr"`
				Dd *int64   `json:"dd"`
			} `json:"p"`
		} `json:"item"`
		Checklist []struct {
			E string `json:"e"`
			P struct {
				Tt string   `json:"tt"`
				Ix int      `json:"ix"`
				Ts []string `json:"ts"`
			} `json:"p"`
		} `json:"checklist"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
		t.Fatalf("unmarshal dry-run content: %v", err)
	}
	if payload.UUID != callerUUID {
		t.Fatalf("uuid = %s, want caller-supplied %s", payload.UUID, callerUUID)
	}
	p := payload.Item.P
	if len(p.Pr) != 1 || p.Pr[0] != projectUUID || len(p.Tg) != 1 || p.Tg[0] != tagUUID {
		t.Fatalf("pr/tg = %v/%v", p.Pr, p.Tg)
	}
	// scheduled with default when=inbox: dates set and st promoted to 1
	// (newTaskCreatePayload's scheduled rule), deadline set.
	if p.St != 1 || p.Sr == nil || p.Dd == nil {
		t.Fatalf("st/sr/dd = %d/%v/%v, want scheduled-anytime with deadline", p.St, p.Sr, p.Dd)
	}
	// Inline checklist: empty titles skipped, 1-based distinct positive ix,
	// items reference the task.
	if len(payload.Checklist) != 2 {
		t.Fatalf("checklist = %d items, want 2", len(payload.Checklist))
	}
	for i, item := range payload.Checklist {
		if item.E != "ChecklistItem3" || item.P.Ix != i+1 || len(item.P.Ts) != 1 || item.P.Ts[0] != callerUUID {
			t.Fatalf("checklist[%d] = %+v", i, item)
		}
	}

	if _, err := server.createTask(createTaskArgs{Title: "X", UUID: "not-canonical", DryRun: true}); err == nil {
		t.Fatal("non-canonical caller uuid should be rejected")
	}
	if _, err := server.createTask(createTaskArgs{Title: "X", Project: "bogus", DryRun: true}); err == nil {
		t.Fatal("non-canonical project should be rejected")
	}
	if _, err := server.createTask(createTaskArgs{Title: "X", Tags: []string{"bogus"}, DryRun: true}); err == nil {
		t.Fatal("non-canonical tag should be rejected")
	}
}

func TestCreateTaskWithChecklistCommitsSequentially(t *testing.T) {
	var commits int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version/1/history/history-id/items":
			fmt.Fprint(w, `{"items":[],"current-item-index":3,"schema":301}`)
		case "/version/1/history/history-id/commit":
			commits++
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode commit body: %v", err)
			}
			if len(body) != 1 {
				t.Errorf("commit body has %d entries, want 1", len(body))
			}
			fmt.Fprintf(w, `{"server-head-index":%d}`, 3+commits)
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client := thingscloud.New(ts.URL, "test@example.com", "secret")
	server := &mcpServer{client: client, history: client.HistoryWithID("history-id")}

	result, err := server.createTask(createTaskArgs{Title: "With list", Checklist: []string{"A", "B"}})
	if err != nil {
		t.Fatalf("createTask failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("createTask returned tool error: %#v", result)
	}
	if commits != 3 {
		t.Fatalf("commits = %d, want 3 (task + 2 checklist items, one each)", commits)
	}
}

func TestEditTaskFullOptionsFollowCLIConventions(t *testing.T) {
	server := &mcpServer{}
	taskUUID := thingscloud.NewUUID()
	projectUUID := thingscloud.NewUUID()
	headingUUID := thingscloud.NewUUID()

	type editPayload struct {
		Pr  []string        `json:"pr"`
		Agr []string        `json:"agr"`
		St  *int            `json:"st"`
		Sr  json.RawMessage `json:"sr"`
		Tir json.RawMessage `json:"tir"`
		Dd  *int64          `json:"dd"`
	}
	decode := func(t *testing.T, result toolResult) editPayload {
		t.Helper()
		var payload struct {
			Item struct {
				P editPayload `json:"p"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
			t.Fatalf("unmarshal dry-run content: %v", err)
		}
		return payload.Item.P
	}

	// scheduled without when: dates set and start date applied (cmdEdit
	// convention: Scheduled + ScheduleDate).
	result, err := server.editTask(editTaskArgs{UUID: taskUUID, Scheduled: "2026-09-01", DryRun: true})
	if err != nil {
		t.Fatalf("edit scheduled failed: %v", err)
	}
	if p := decode(t, result); len(p.Sr) == 0 || string(p.Sr) == "null" {
		t.Fatalf("sr = %s, want a date", p.Sr)
	}

	// project without explicit schedule: auto-Anytime would clobber nothing,
	// pr set and st=1.
	result, err = server.editTask(editTaskArgs{UUID: taskUUID, Project: projectUUID, DryRun: true})
	if err != nil {
		t.Fatalf("edit project failed: %v", err)
	}
	if p := decode(t, result); len(p.Pr) != 1 || p.Pr[0] != projectUUID || p.St == nil || *p.St != 1 {
		t.Fatalf("edit project payload = %+v", p)
	}

	// project with explicit scheduled: no auto-Anytime, the scheduled date
	// survives (hasExplicitSchedule convention).
	result, err = server.editTask(editTaskArgs{UUID: taskUUID, Project: projectUUID, Scheduled: "2026-09-01", DryRun: true})
	if err != nil {
		t.Fatalf("edit project+scheduled failed: %v", err)
	}
	if p := decode(t, result); string(p.Sr) == "null" || len(p.Sr) == 0 {
		t.Fatalf("sr = %s, want scheduled date to survive container attach", p.Sr)
	}

	// heading and deadline together.
	result, err = server.editTask(editTaskArgs{UUID: taskUUID, Heading: headingUUID, Deadline: "2026-09-15", DryRun: true})
	if err != nil {
		t.Fatalf("edit heading failed: %v", err)
	}
	if p := decode(t, result); len(p.Agr) != 1 || p.Agr[0] != headingUUID || p.Dd == nil {
		t.Fatalf("edit heading payload = %+v", p)
	}

	if _, err := server.editTask(editTaskArgs{UUID: taskUUID, Heading: "bogus", DryRun: true}); err == nil {
		t.Fatal("non-canonical heading should be rejected")
	}
}

func TestCreateHeadingAreaTagFollowCLIConventions(t *testing.T) {
	server := &mcpServer{}
	projectUUID := thingscloud.NewUUID()
	tagUUID := thingscloud.NewUUID()
	parentTag := thingscloud.NewUUID()

	// Heading: tp=2, structural st=1, inside the project.
	result, err := server.createHeading("Phase 1", projectUUID, true)
	if err != nil {
		t.Fatalf("createHeading failed: %v", err)
	}
	var heading struct {
		UUID string `json:"uuid"`
		Item struct {
			E string `json:"e"`
			P struct {
				Tp int      `json:"tp"`
				St int      `json:"st"`
				Pr []string `json:"pr"`
			} `json:"p"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &heading); err != nil {
		t.Fatalf("unmarshal heading: %v", err)
	}
	if heading.Item.E != "Task6" || heading.Item.P.Tp != 2 || heading.Item.P.St != 1 {
		t.Fatalf("heading payload = %+v, want tp=2 st=1", heading.Item.P)
	}
	if len(heading.Item.P.Pr) != 1 || heading.Item.P.Pr[0] != projectUUID {
		t.Fatalf("heading pr = %v, want [%s]", heading.Item.P.Pr, projectUUID)
	}
	if err := thingscloud.ValidateUUID(heading.UUID); err != nil {
		t.Fatalf("heading uuid not canonical: %v", err)
	}
	if _, err := server.createHeading("Phase 1", "", true); err == nil {
		t.Fatal("heading without project should be rejected")
	}

	// Area: {tt, ix:0, tg, xx} as Area3, exactly the CLI payload.
	result, err = server.createArea("Home", []string{tagUUID}, true)
	if err != nil {
		t.Fatalf("createArea failed: %v", err)
	}
	var area struct {
		Item struct {
			T int    `json:"t"`
			E string `json:"e"`
			P struct {
				Tt string   `json:"tt"`
				Ix int      `json:"ix"`
				Tg []string `json:"tg"`
			} `json:"p"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &area); err != nil {
		t.Fatalf("unmarshal area: %v", err)
	}
	if area.Item.E != "Area3" || area.Item.T != 0 || area.Item.P.Tt != "Home" || area.Item.P.Ix != 0 {
		t.Fatalf("area payload = %+v", area.Item)
	}
	if len(area.Item.P.Tg) != 1 || area.Item.P.Tg[0] != tagUUID {
		t.Fatalf("area tg = %v", area.Item.P.Tg)
	}
	if _, err := server.createArea("Home", []string{"bogus"}, true); err == nil {
		t.Fatal("non-canonical area tag should be rejected")
	}

	// Tag: Tag4 with the CLI's negative ix convention, null shorthand by
	// default, parent in pn.
	result, err = server.createTag("@errand", "e", parentTag, true)
	if err != nil {
		t.Fatalf("createTag failed: %v", err)
	}
	var tag struct {
		Item struct {
			E string `json:"e"`
			P struct {
				Tt string   `json:"tt"`
				Ix int      `json:"ix"`
				Sh *string  `json:"sh"`
				Pn []string `json:"pn"`
			} `json:"p"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &tag); err != nil {
		t.Fatalf("unmarshal tag: %v", err)
	}
	if tag.Item.E != "Tag4" || tag.Item.P.Tt != "@errand" {
		t.Fatalf("tag payload = %+v", tag.Item)
	}
	if tag.Item.P.Ix >= 0 {
		t.Fatalf("tag ix = %d, want negative (Things tag convention)", tag.Item.P.Ix)
	}
	if tag.Item.P.Sh == nil || *tag.Item.P.Sh != "e" || len(tag.Item.P.Pn) != 1 || tag.Item.P.Pn[0] != parentTag {
		t.Fatalf("tag sh/pn = %v/%v", tag.Item.P.Sh, tag.Item.P.Pn)
	}
	if _, err := server.createTag("@errand", "", "bogus", true); err == nil {
		t.Fatal("non-canonical parent tag should be rejected")
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
	result, err := server.editTask(editTaskArgs{UUID: taskUUID, Title: "New title", Note: "new note", When: "anytime", DryRun: true})
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
	if _, err := server.editTask(editTaskArgs{UUID: "not-a-uuid", Title: "T", DryRun: true}); err == nil {
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

func TestCreateProjectDryRunFollowsCLIConventions(t *testing.T) {
	server := &mcpServer{}
	areaUUID := thingscloud.NewUUID()

	result, err := server.createProject("New project", "a note", areaUUID, true)
	if err != nil {
		t.Fatalf("createProject dry-run failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("dry-run returned tool error: %#v", result)
	}
	var payload struct {
		Status string `json:"status"`
		UUID   string `json:"uuid"`
		Item   struct {
			T int    `json:"t"`
			E string `json:"e"`
			P struct {
				Tp int             `json:"tp"`
				St int             `json:"st"`
				Ar []string        `json:"ar"`
				Tt string          `json:"tt"`
				Md json.RawMessage `json:"md"`
				Nt struct {
					Value string `json:"v"`
				} `json:"nt"`
			} `json:"p"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
		t.Fatalf("unmarshal dry-run content: %v", err)
	}
	if payload.Status != "dry-run" {
		t.Fatalf("status = %q, want dry-run", payload.Status)
	}
	if err := thingscloud.ValidateUUID(payload.UUID); err != nil {
		t.Fatalf("generated project uuid %q is not canonical: %v", payload.UUID, err)
	}
	if payload.Item.T != 0 || payload.Item.E != "Task6" {
		t.Fatalf("item = %#v, want Task6 create", payload.Item)
	}
	// CLI create --type project: tp=1, and the structural rule forces st=1
	// (a structural item with st=0 crashes Things.app).
	if payload.Item.P.Tp != 1 {
		t.Fatalf("tp = %d, want 1 (project)", payload.Item.P.Tp)
	}
	if payload.Item.P.St != 1 {
		t.Fatalf("st = %d, want 1 (structural, never inbox)", payload.Item.P.St)
	}
	if len(payload.Item.P.Ar) != 1 || payload.Item.P.Ar[0] != areaUUID {
		t.Fatalf("ar = %v, want [%s]", payload.Item.P.Ar, areaUUID)
	}
	if payload.Item.P.Tt != "New project" || payload.Item.P.Nt.Value != "a note" {
		t.Fatalf("title/note = %q/%q, want New project/a note", payload.Item.P.Tt, payload.Item.P.Nt.Value)
	}
	if string(payload.Item.P.Md) != "null" {
		t.Fatalf("md = %s, want null on creates", payload.Item.P.Md)
	}

	if _, err := server.createProject("", "", "", true); err == nil {
		t.Fatal("empty title should be rejected")
	}
	if _, err := server.createProject("P", "", "not-an-area", true); err == nil {
		t.Fatal("non-canonical area uuid should be rejected")
	}
	if _, err := server.callTool(json.RawMessage(`{"name":"create_project","arguments":{"title":"P","heading":"x","dry_run":true}}`)); err == nil {
		t.Fatal("unknown create_project field should be rejected")
	}
}

func TestEditTaskTagsFollowCLIConvention(t *testing.T) {
	server := &mcpServer{}
	taskUUID := thingscloud.NewUUID()
	tagOne := thingscloud.NewUUID()
	tagTwo := thingscloud.NewUUID()

	result, err := server.editTask(editTaskArgs{UUID: taskUUID, Tags: []string{tagOne, " " + tagTwo}, DryRun: true})
	if err != nil {
		t.Fatalf("editTask with tags failed: %v", err)
	}
	var payload struct {
		Status string `json:"status"`
		Item   struct {
			P struct {
				Tg []string `json:"tg"`
			} `json:"p"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
		t.Fatalf("unmarshal dry-run content: %v", err)
	}
	if payload.Status != "dry-run" {
		t.Fatalf("status = %q, want dry-run", payload.Status)
	}
	// CLI edit --tags: validated (trimmed) tag UUIDs replace the whole tag
	// set via the tg field.
	if len(payload.Item.P.Tg) != 2 || payload.Item.P.Tg[0] != tagOne || payload.Item.P.Tg[1] != tagTwo {
		t.Fatalf("tg = %v, want [%s %s]", payload.Item.P.Tg, tagOne, tagTwo)
	}

	if _, err := server.editTask(editTaskArgs{UUID: taskUUID, Tags: []string{"not-a-tag"}, DryRun: true}); err == nil {
		t.Fatal("non-canonical tag uuid should be rejected")
	}

	// Batch edit follows the same convention.
	batchResult, err := server.batchTasks([]batchTaskOp{
		{Cmd: "edit", UUID: taskUUID, Tags: []string{tagOne}},
	}, true)
	if err != nil {
		t.Fatalf("batch edit with tags failed: %v", err)
	}
	var batchPayload struct {
		Items []struct {
			P struct {
				Tg []string `json:"tg"`
			} `json:"p"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(batchResult.Content[0].Text), &batchPayload); err != nil {
		t.Fatalf("unmarshal batch dry-run content: %v", err)
	}
	if len(batchPayload.Items) != 1 || len(batchPayload.Items[0].P.Tg) != 1 || batchPayload.Items[0].P.Tg[0] != tagOne {
		t.Fatalf("batch tg = %#v, want [%s]", batchPayload.Items, tagOne)
	}
	if _, err := server.batchTasks([]batchTaskOp{
		{Cmd: "edit", UUID: taskUUID, Tags: []string{"zzzzzzzzzzzzzzzzzzzzzz"}},
	}, true); err == nil {
		t.Fatal("batch edit with over-range tag uuid should be rejected")
	}
}

func TestPurgeTaskWritesTombstone(t *testing.T) {
	server := &mcpServer{}
	taskUUID := thingscloud.NewUUID()

	result, err := server.purgeTask(taskUUID, true)
	if err != nil {
		t.Fatalf("purgeTask dry-run failed: %v", err)
	}
	var payload struct {
		Status string `json:"status"`
		UUID   string `json:"uuid"`
		Item   struct {
			T int    `json:"t"`
			E string `json:"e"`
			P struct {
				Dloid string   `json:"dloid"`
				Dld   *float64 `json:"dld"`
			} `json:"p"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
		t.Fatalf("unmarshal dry-run content: %v", err)
	}
	// CLI purge: a Tombstone2 CREATE whose dloid names the target and whose
	// envelope uuid is a fresh identifier, not the task's.
	if payload.Item.E != "Tombstone2" || payload.Item.T != 0 {
		t.Fatalf("item = %+v, want Tombstone2 create", payload.Item)
	}
	if payload.Item.P.Dloid != taskUUID || payload.Item.P.Dld == nil {
		t.Fatalf("tombstone payload = %+v, want dloid=%s with dld", payload.Item.P, taskUUID)
	}

	if _, err := server.purgeTask("task-1", true); err == nil {
		t.Fatal("non-canonical uuid should be rejected even for purge dry-run")
	}
	if _, err := server.batchTasks([]batchTaskOp{{Cmd: "purge", UUID: taskUUID}}, true); err == nil {
		t.Fatal("purge must not be available as a batch op")
	}

	// The tool description must carry the permanence warning for hosts.
	for _, tool := range tools() {
		if tool.Name != "purge_task" {
			continue
		}
		for _, want := range []string{"PERMANENTLY", "unrecoverable", "human confirmation"} {
			if !strings.Contains(strings.ToLower(tool.Description), strings.ToLower(want)) {
				t.Fatalf("purge_task description missing %q: %s", want, tool.Description)
			}
		}
	}
}

func TestMoveTaskDryRunFollowsCLIConventions(t *testing.T) {
	server := &mcpServer{}
	taskUUID := thingscloud.NewUUID()
	projectUUID := thingscloud.NewUUID()
	areaUUID := thingscloud.NewUUID()
	headingUUID := thingscloud.NewUUID()

	type movePayload struct {
		Pr  []string        `json:"pr"`
		Ar  []string        `json:"ar"`
		Agr []string        `json:"agr"`
		St  *int            `json:"st"`
		Sr  json.RawMessage `json:"sr"`
		Ix  *int            `json:"ix"`
	}
	decode := func(t *testing.T, result toolResult) movePayload {
		t.Helper()
		var payload struct {
			Status string `json:"status"`
			UUID   string `json:"uuid"`
			Item   struct {
				T int         `json:"t"`
				E string      `json:"e"`
				P movePayload `json:"p"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
			t.Fatalf("unmarshal dry-run content: %v", err)
		}
		if payload.Status != "dry-run" || payload.UUID != taskUUID {
			t.Fatalf("payload = %#v, want dry-run for %s", payload, taskUUID)
		}
		if payload.Item.T != 1 || payload.Item.E != "Task6" {
			t.Fatalf("item = %#v, want Task6 update", payload.Item)
		}
		if payload.Item.P.Ix != nil {
			t.Fatalf("move must not touch ix, got %v", *payload.Item.P.Ix)
		}
		return payload.Item.P
	}

	t.Run("project", func(t *testing.T) {
		result, err := server.moveTask(taskUUID, projectUUID, "", "", false, true)
		if err != nil {
			t.Fatalf("moveTask failed: %v", err)
		}
		p := decode(t, result)
		// CLI buildBatchMoveToProject: Project(uuid).Anytime().
		if len(p.Pr) != 1 || p.Pr[0] != projectUUID {
			t.Fatalf("pr = %v, want [%s]", p.Pr, projectUUID)
		}
		if p.St == nil || *p.St != 1 {
			t.Fatalf("st = %v, want 1 (anytime)", p.St)
		}
		if string(p.Sr) != "null" {
			t.Fatalf("sr = %s, want explicit null", p.Sr)
		}
	})

	t.Run("project with heading", func(t *testing.T) {
		result, err := server.moveTask(taskUUID, projectUUID, "", headingUUID, false, true)
		if err != nil {
			t.Fatalf("moveTask failed: %v", err)
		}
		p := decode(t, result)
		if len(p.Pr) != 1 || p.Pr[0] != projectUUID {
			t.Fatalf("pr = %v, want [%s]", p.Pr, projectUUID)
		}
		// CLI edit --heading: agr carries the heading uuid.
		if len(p.Agr) != 1 || p.Agr[0] != headingUUID {
			t.Fatalf("agr = %v, want [%s]", p.Agr, headingUUID)
		}
	})

	t.Run("area", func(t *testing.T) {
		result, err := server.moveTask(taskUUID, "", areaUUID, "", false, true)
		if err != nil {
			t.Fatalf("moveTask failed: %v", err)
		}
		p := decode(t, result)
		// CLI buildBatchMoveToArea: Area(uuid).Anytime().
		if len(p.Ar) != 1 || p.Ar[0] != areaUUID {
			t.Fatalf("ar = %v, want [%s]", p.Ar, areaUUID)
		}
		if p.St == nil || *p.St != 1 {
			t.Fatalf("st = %v, want 1 (anytime)", p.St)
		}
	})

	t.Run("inbox", func(t *testing.T) {
		result, err := server.moveTask(taskUUID, "", "", "", true, true)
		if err != nil {
			t.Fatalf("moveTask failed: %v", err)
		}
		p := decode(t, result)
		// CLI edit --when inbox: Schedule(0, nil, nil).
		if p.St == nil || *p.St != 0 {
			t.Fatalf("st = %v, want 0 (inbox)", p.St)
		}
		if string(p.Sr) != "null" {
			t.Fatalf("sr = %s, want explicit null", p.Sr)
		}
		if len(p.Pr) != 0 && p.Pr != nil {
			t.Fatalf("inbox move must not set pr, got %v", p.Pr)
		}
	})
}

func TestMoveTaskValidation(t *testing.T) {
	server := &mcpServer{}
	taskUUID := thingscloud.NewUUID()
	projectUUID := thingscloud.NewUUID()
	areaUUID := thingscloud.NewUUID()

	if _, err := server.moveTask(taskUUID, "", "", "", false, true); err == nil {
		t.Fatal("no destination should be rejected")
	}
	if _, err := server.moveTask(taskUUID, projectUUID, areaUUID, "", false, true); err == nil {
		t.Fatal("two destinations should be rejected")
	}
	if _, err := server.moveTask(taskUUID, projectUUID, "", "", true, true); err == nil {
		t.Fatal("project plus inbox should be rejected")
	}
	if _, err := server.moveTask(taskUUID, "", areaUUID, thingscloud.NewUUID(), false, true); err == nil {
		t.Fatal("heading with area should be rejected")
	}
	if _, err := server.moveTask(taskUUID, "", "", thingscloud.NewUUID(), true, true); err == nil {
		t.Fatal("heading with inbox should be rejected")
	}
	if _, err := server.moveTask("task-1", projectUUID, "", "", false, true); err == nil {
		t.Fatal("non-canonical task uuid should be rejected")
	}
	if _, err := server.moveTask(taskUUID, "not-a-uuid", "", "", false, true); err == nil {
		t.Fatal("non-canonical project uuid should be rejected")
	}
	if _, err := server.moveTask(taskUUID, projectUUID, "", "zzzzzzzzzzzzzzzzzzzzzz", false, true); err == nil {
		t.Fatal("non-canonical heading uuid should be rejected")
	}
	if _, err := server.moveTask(taskUUID, "", "zzzzzzzzzzzzzzzzzzzzzz", "", false, true); err == nil {
		t.Fatal("non-canonical area uuid should be rejected")
	}

	// Unknown argument fields must fail loudly, not silently drop.
	if _, err := server.callTool(json.RawMessage(`{"name":"move_task","arguments":{"uuid":"` + taskUUID + `","projct":"` + projectUUID + `","dry_run":true}}`)); err == nil {
		t.Fatal("unknown move_task field should be rejected")
	}
}

func TestMoveTaskCommitsOneWrite(t *testing.T) {
	var commits int
	taskUUID := thingscloud.NewUUID()
	projectUUID := thingscloud.NewUUID()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version/1/history/history-id/items":
			fmt.Fprint(w, `{"items":[],"current-item-index":3,"schema":301}`)
		case "/version/1/history/history-id/commit":
			commits++
			if got := r.URL.Query().Get("ancestor-index"); got != "3" {
				t.Errorf("ancestor-index = %s, want 3", got)
			}
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode commit body: %v", err)
			}
			if len(body) != 1 {
				t.Errorf("commit body has %d entries, want 1", len(body))
			}
			if _, ok := body[taskUUID]; !ok {
				t.Errorf("commit body missing task %s", taskUUID)
			}
			fmt.Fprint(w, `{"server-head-index":4}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client := thingscloud.New(ts.URL, "test@example.com", "secret")
	server := &mcpServer{client: client, history: client.HistoryWithID("history-id")}

	result, err := server.moveTask(taskUUID, projectUUID, "", "", false, false)
	if err != nil {
		t.Fatalf("moveTask failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("moveTask returned tool error: %#v", result)
	}
	if commits != 1 {
		t.Fatalf("commits = %d, want 1", commits)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}
	if payload["status"] != "moved" || payload["uuid"] != taskUUID || payload["project"] != projectUUID {
		t.Fatalf("payload = %#v, want moved to %s", payload, projectUUID)
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

func TestBatchMoveOpsFollowCLIConventions(t *testing.T) {
	server := &mcpServer{}
	taskUUID := thingscloud.NewUUID()
	projectUUID := thingscloud.NewUUID()
	areaUUID := thingscloud.NewUUID()

	result, err := server.batchTasks([]batchTaskOp{
		{Cmd: "move-to-project", UUID: taskUUID, Project: projectUUID},
		{Cmd: "move-to-area", UUID: thingscloud.NewUUID(), Area: areaUUID},
		{Cmd: "move-to-today", UUID: thingscloud.NewUUID()},
	}, true)
	if err != nil {
		t.Fatalf("batch move ops failed: %v", err)
	}
	var payload struct {
		Items []struct {
			T int `json:"t"`
			P struct {
				Pr []string        `json:"pr"`
				Ar []string        `json:"ar"`
				St *int            `json:"st"`
				Sr json.RawMessage `json:"sr"`
			} `json:"p"`
		} `json:"items"`
		Results []map[string]string `json:"results"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
		t.Fatalf("unmarshal dry-run content: %v", err)
	}
	if len(payload.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(payload.Items))
	}
	// move-to-project: Project(uuid)+Anytime, exactly like the CLI builder.
	if p := payload.Items[0].P; len(p.Pr) != 1 || p.Pr[0] != projectUUID || p.St == nil || *p.St != 1 || string(p.Sr) != "null" {
		t.Fatalf("move-to-project payload = %+v, want pr+anytime", p)
	}
	// move-to-area: Area(uuid)+Anytime.
	if p := payload.Items[1].P; len(p.Ar) != 1 || p.Ar[0] != areaUUID || p.St == nil || *p.St != 1 {
		t.Fatalf("move-to-area payload = %+v, want ar+anytime", p)
	}
	// move-to-today: Today() sets st=1 with a concrete date.
	if p := payload.Items[2].P; p.St == nil || *p.St != 1 || string(p.Sr) == "null" || len(p.Sr) == 0 {
		t.Fatalf("move-to-today payload = %+v, want st=1 with date", p)
	}
	if payload.Results[0]["cmd"] != "move-to-project" || payload.Results[0]["project"] != projectUUID {
		t.Fatalf("results[0] = %v", payload.Results[0])
	}

	// Required-field and validation failures.
	if _, err := server.batchTasks([]batchTaskOp{{Cmd: "move-to-project", UUID: taskUUID}}, true); err == nil {
		t.Fatal("move-to-project without project should be rejected")
	}
	if _, err := server.batchTasks([]batchTaskOp{{Cmd: "move-to-area", UUID: taskUUID}}, true); err == nil {
		t.Fatal("move-to-area without area should be rejected")
	}
	if _, err := server.batchTasks([]batchTaskOp{{Cmd: "move-to-today"}}, true); err == nil {
		t.Fatal("move-to-today without uuid should be rejected")
	}
	if _, err := server.batchTasks([]batchTaskOp{{Cmd: "move-to-project", UUID: taskUUID, Project: "bogus"}}, true); err == nil {
		t.Fatal("move-to-project with non-canonical project should be rejected")
	}
}

func TestBatchCreateAndEditContainerFields(t *testing.T) {
	server := &mcpServer{}
	taskUUID := thingscloud.NewUUID()
	projectUUID := thingscloud.NewUUID()
	areaUUID := thingscloud.NewUUID()
	headingUUID := thingscloud.NewUUID()

	result, err := server.batchTasks([]batchTaskOp{
		{Cmd: "create", Title: "In project", Project: projectUUID},
		{Cmd: "create", Title: "A heading", Type: "heading", Project: projectUUID},
		{Cmd: "edit", UUID: taskUUID, Heading: headingUUID},
		{Cmd: "edit", UUID: thingscloud.NewUUID(), Area: areaUUID, When: "someday"},
	}, true)
	if err != nil {
		t.Fatalf("batch container ops failed: %v", err)
	}
	var payload struct {
		Items []struct {
			P struct {
				Tp  *int     `json:"tp"`
				Pr  []string `json:"pr"`
				Ar  []string `json:"ar"`
				Agr []string `json:"agr"`
				St  *int     `json:"st"`
			} `json:"p"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
		t.Fatalf("unmarshal dry-run content: %v", err)
	}
	// create --project: pr set, and the builder moves it out of Inbox (st=1).
	if p := payload.Items[0].P; len(p.Pr) != 1 || p.Pr[0] != projectUUID || p.St == nil || *p.St != 1 {
		t.Fatalf("create-with-project payload = %+v", p)
	}
	// create --type heading: tp=2, structural st=1.
	if p := payload.Items[1].P; p.Tp == nil || *p.Tp != 2 || p.St == nil || *p.St != 1 {
		t.Fatalf("create-heading payload = %+v", p)
	}
	// edit --heading with no explicit schedule: agr plus auto-Anytime.
	if p := payload.Items[2].P; len(p.Agr) != 1 || p.Agr[0] != headingUUID || p.St == nil || *p.St != 1 {
		t.Fatalf("edit-heading payload = %+v", p)
	}
	// edit --area with explicit when: area set, when wins (st=2 someday).
	if p := payload.Items[3].P; len(p.Ar) != 1 || p.Ar[0] != areaUUID || p.St == nil || *p.St != 2 {
		t.Fatalf("edit-area-with-when payload = %+v", p)
	}

	if _, err := server.batchTasks([]batchTaskOp{{Cmd: "create", Title: "X", Type: "explosion"}}, true); err == nil {
		t.Fatal("unknown type should be rejected")
	}
	if _, err := server.batchTasks([]batchTaskOp{{Cmd: "create", Title: "X", Project: "bogus"}}, true); err == nil {
		t.Fatal("create with non-canonical project should be rejected")
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

func TestWriteRetriesOnceOnCommitConflict(t *testing.T) {
	var commits, syncs int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version/1/history/history-id/items":
			syncs++
			fmt.Fprint(w, `{"items":[],"current-item-index":7,"schema":301}`)
		case "/version/1/history/history-id/commit":
			commits++
			if commits == 1 {
				w.WriteHeader(http.StatusConflict)
				return
			}
			// The retry must carry the re-synced ancestor-index.
			if got := r.URL.Query().Get("ancestor-index"); got != "7" {
				t.Errorf("retry ancestor-index = %s, want 7", got)
			}
			fmt.Fprint(w, `{"server-head-index":8}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client := thingscloud.New(ts.URL, "test@example.com", "secret")
	server := &mcpServer{client: client, history: client.HistoryWithID("history-id")}

	result, err := server.createTask(createTaskArgs{Title: "Conflicted", When: "anytime"})
	if err != nil {
		t.Fatalf("createTask failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("createTask returned tool error after conflict retry: %#v", result)
	}
	if commits != 2 {
		t.Fatalf("commits = %d, want 2 (original + one retry)", commits)
	}
	if syncs != 2 {
		t.Fatalf("syncs = %d, want 2 (initial + re-sync before retry)", syncs)
	}
}

func TestWriteDoesNotRetryConflictTwice(t *testing.T) {
	var commits int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version/1/history/history-id/items":
			fmt.Fprint(w, `{"items":[],"current-item-index":7,"schema":301}`)
		case "/version/1/history/history-id/commit":
			commits++
			w.WriteHeader(http.StatusConflict)
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client := thingscloud.New(ts.URL, "test@example.com", "secret")
	server := &mcpServer{client: client, history: client.HistoryWithID("history-id")}

	result, err := server.createTask(createTaskArgs{Title: "Still conflicted", When: "anytime"})
	if err != nil {
		t.Fatalf("createTask failed: %v", err)
	}
	if !result.IsError {
		t.Fatalf("persistent conflict should surface a tool error, got %#v", result)
	}
	if commits != 2 {
		t.Fatalf("commits = %d, want exactly 2 (no second retry)", commits)
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

func TestEditTaskDeadlineNoneClearsWithNull(t *testing.T) {
	u := newTaskUpdate()
	u.ClearDeadline()
	bs, err := json.Marshal(u.build())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(bs), `"dd":null`) {
		t.Fatalf("expected explicit dd:null in update payload, got %s", bs)
	}
}
