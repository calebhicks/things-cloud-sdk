package main

import (
	"encoding/json"
	"testing"
)

func TestToolJSONEmptySliceSerializesAsArray(t *testing.T) {
	// contracts.md documents every MCP list result as a JSON array. The list
	// functions must initialize empty slices so empty results serialize as []
	// rather than null (a nil slice marshals to null).
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

func TestCreateTaskDryRunDoesNotRequireCloud(t *testing.T) {
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
	if payload.UUID == "" {
		t.Fatal("dry-run uuid is empty")
	}
	if payload.Item.E != "Task6" {
		t.Fatalf("item kind = %q, want Task6", payload.Item.E)
	}
}

func TestCompleteTaskDryRunDoesNotRequireCloud(t *testing.T) {
	server := &mcpServer{}
	result, err := server.completeTask("task-1", true)
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
	if payload.Status != "dry-run" || payload.UUID != "task-1" {
		t.Fatalf("payload = %#v, want dry-run for task-1", payload)
	}
}

func TestEditTaskDryRunDoesNotRequireCloud(t *testing.T) {
	server := &mcpServer{}
	result, err := server.editTask("task-1", "New title", "new note", "anytime", true)
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
	if payload.Status != "dry-run" || payload.UUID != "task-1" {
		t.Fatalf("payload = %#v, want dry-run for task-1", payload)
	}
	if payload.Item.E != "Task6" || payload.Item.P.Title != "New title" || payload.Item.P.St != 1 {
		t.Fatalf("item payload = %#v, want Task6 title and anytime schedule", payload.Item)
	}
}

func TestAddChecklistDryRunDoesNotRequireCloud(t *testing.T) {
	server := &mcpServer{}
	result, err := server.addChecklist("task-1", []string{"One", "Two"}, true)
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
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
		t.Fatalf("unmarshal dry-run content failed: %v", err)
	}
	if payload.Status != "dry-run" || payload.UUID != "task-1" || len(payload.Items) != 2 {
		t.Fatalf("payload = %#v, want two dry-run checklist items", payload)
	}
	if payload.Items[0].E != "ChecklistItem3" {
		t.Fatalf("item kind = %q, want ChecklistItem3", payload.Items[0].E)
	}
}
