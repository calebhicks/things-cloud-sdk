// Command things-mcp is a stdio MCP server exposing safe Things Cloud task
// tools. Reads go through the shared JSON state cache; writes are dry-run
// previewable and committed one item per request.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	thingscloud "github.com/arthursoares/things-cloud-sdk"
)

const protocolVersion = "2025-06-18"
const serverVersion = "0.4.0"
const maxBatchOperations = 50

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content           []textContent `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError,omitempty"`
}

type toolDefinition struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type mcpServer struct {
	client  *thingscloud.Client
	history *thingscloud.History
	// endpoint overrides thingscloud.APIEndpoint in tests.
	endpoint string
}

func main() {
	server := &mcpServer{}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	enc := json.NewEncoder(os.Stdout)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			writeRPCError(enc, nil, -32700, "parse error")
			continue
		}

		resp, ok := server.handle(req)
		if !ok {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			fmt.Fprintf(os.Stderr, "write response: %v\n", err)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "read stdin: %v\n", err)
	}
}

func (s *mcpServer) handle(req rpcRequest) (rpcResponse, bool) {
	switch req.Method {
	case "initialize":
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities": map[string]any{
					"tools": map[string]any{"listChanged": false},
				},
				"serverInfo": map[string]string{
					"name":    "things-cloud-sdk",
					"title":   "Things Cloud SDK MCP",
					"version": serverVersion,
				},
				"instructions": "Use these tools to read and safely update Things Cloud tasks. Destructive actions should be confirmed by the host application.",
			},
		}, true
	case "notifications/initialized":
		return rpcResponse{}, false
	case "ping":
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}, true
	case "tools/list":
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": tools()}}, true
	case "tools/call":
		result, err := s.callTool(req.Params)
		if err != nil {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: err.Error()}}, true
		}
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}, true
	default:
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: "method not found"}}, true
	}
}

func writeRPCError(enc *json.Encoder, id json.RawMessage, code int, message string) {
	_ = enc.Encode(rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: message},
	})
}

func tools() []toolDefinition {
	return []toolDefinition{
		{
			Name:        "list_tasks",
			Title:       "List Tasks",
			Description: "List active Things tasks by view. View can be all, today, inbox, anytime, someday, or upcoming.",
			InputSchema: objectSchema(map[string]any{
				"view": map[string]any{
					"type":        "string",
					"description": "Task view to list.",
					"enum":        []string{"all", "today", "inbox", "anytime", "someday", "upcoming"},
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of tasks to return.",
					"minimum":     1,
					"maximum":     200,
				},
			}, nil),
		},
		{
			Name:        "search_tasks",
			Title:       "Search Tasks",
			Description: "Search active Things tasks by title and note.",
			InputSchema: objectSchema(map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Case-insensitive search query.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of tasks to return.",
					"minimum":     1,
					"maximum":     200,
				},
			}, []string{"query"}),
		},
		{
			Name:        "create_task",
			Title:       "Create Task",
			Description: "Create a Things task. Supports when values inbox, today, anytime, and someday.",
			InputSchema: objectSchema(map[string]any{
				"title": map[string]any{
					"type":        "string",
					"description": "Task title.",
				},
				"note": map[string]any{
					"type":        "string",
					"description": "Optional task note.",
				},
				"when": map[string]any{
					"type":        "string",
					"description": "Schedule bucket.",
					"enum":        []string{"inbox", "today", "anytime", "someday"},
				},
				"scheduled": stringProp("Scheduled (start) date as YYYY-MM-DD."),
				"deadline":  stringProp("Deadline date as YYYY-MM-DD."),
				"project":   stringProp("Project UUID to create the task in."),
				"area":      stringProp("Area UUID to create the task in."),
				"heading":   stringProp("Heading UUID to create the task under."),
				"tags": map[string]any{
					"type":        "array",
					"description": "Tag UUIDs.",
					"items":       map[string]any{"type": "string"},
				},
				"checklist": map[string]any{
					"type":        "array",
					"description": "Checklist item titles to create with the task.",
					"items":       map[string]any{"type": "string"},
				},
				"uuid": stringProp("Optional caller-supplied Things Base58 task UUID; generated when omitted."),
				"dry_run": map[string]any{
					"type":        "boolean",
					"description": "Build and return the payload without writing to Things Cloud.",
				},
			}, []string{"title"}),
		},
		{
			Name:        "create_project",
			Title:       "Create Project",
			Description: "Create a Things project, optionally inside an area.",
			InputSchema: objectSchema(map[string]any{
				"title":   stringProp("Project title."),
				"note":    stringProp("Optional project note."),
				"area":    stringProp("Area UUID to create the project in."),
				"dry_run": dryRunProp(),
			}, []string{"title"}),
		},
		{
			Name:        "create_heading",
			Title:       "Create Heading",
			Description: "Create a heading inside a project to group its tasks.",
			InputSchema: objectSchema(map[string]any{
				"title":   stringProp("Heading title."),
				"project": stringProp("Project UUID the heading belongs to."),
				"dry_run": dryRunProp(),
			}, []string{"title", "project"}),
		},
		{
			Name:        "create_area",
			Title:       "Create Area",
			Description: "Create a Things area.",
			InputSchema: objectSchema(map[string]any{
				"title": stringProp("Area title."),
				"tags": map[string]any{
					"type":        "array",
					"description": "Tag UUIDs.",
					"items":       map[string]any{"type": "string"},
				},
				"dry_run": dryRunProp(),
			}, []string{"title"}),
		},
		{
			Name:        "create_tag",
			Title:       "Create Tag",
			Description: "Create a Things tag.",
			InputSchema: objectSchema(map[string]any{
				"title":     stringProp("Tag title."),
				"shorthand": stringProp("Optional one-key shorthand."),
				"parent":    stringProp("Optional parent tag UUID."),
				"dry_run":   dryRunProp(),
			}, []string{"title"}),
		},
		{
			Name:        "complete_task",
			Title:       "Complete Task",
			Description: "Mark a Things task as completed.",
			InputSchema: objectSchema(map[string]any{
				"uuid": map[string]any{
					"type":        "string",
					"description": "Task UUID.",
				},
				"dry_run": map[string]any{
					"type":        "boolean",
					"description": "Build and return the payload without writing to Things Cloud.",
				},
			}, []string{"uuid"}),
		},
		{
			Name:        "edit_task",
			Title:       "Edit Task",
			Description: "Edit task title, note, schedule, dates, containers, or tags.",
			InputSchema: objectSchema(map[string]any{
				"uuid":      stringProp("Task UUID."),
				"title":     stringProp("New task title."),
				"note":      stringProp("New task note."),
				"when":      enumProp("Schedule bucket.", []string{"inbox", "today", "anytime", "someday"}),
				"scheduled": stringProp("Scheduled (start) date as YYYY-MM-DD."),
				"deadline":  stringProp("Deadline date as YYYY-MM-DD."),
				"project":   stringProp("Project UUID to file the task in."),
				"area":      stringProp("Area UUID to file the task in."),
				"heading":   stringProp("Heading UUID to file the task under."),
				"tags": map[string]any{
					"type":        "array",
					"description": "Tag UUIDs. Replaces the task's whole tag set.",
					"items":       map[string]any{"type": "string"},
				},
				"dry_run": dryRunProp(),
			}, []string{"uuid"}),
		},
		{
			Name:        "batch_tasks",
			Title:       "Batch Tasks",
			Description: "Create, edit, complete, trash, or move up to 50 tasks. Items are committed sequentially, one write request each.",
			InputSchema: objectSchema(map[string]any{
				"operations": map[string]any{
					"type":        "array",
					"description": "Batch operations, committed sequentially.",
					"minItems":    1,
					"maxItems":    maxBatchOperations,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"cmd":       enumProp("Operation.", []string{"create", "edit", "complete", "trash", "move-to-today", "move-to-project", "move-to-area"}),
							"uuid":      stringProp("Task UUID. Optional for create (generated when omitted), required otherwise."),
							"title":     stringProp("Task title. Required for create."),
							"note":      stringProp("Task note."),
							"when":      enumProp("Schedule bucket.", []string{"inbox", "today", "anytime", "someday"}),
							"scheduled": stringProp("Scheduled date as YYYY-MM-DD."),
							"deadline":  stringProp("Deadline date as YYYY-MM-DD."),
							"tags": map[string]any{
								"type":        "array",
								"description": "Tag UUIDs for create or edit. Replaces the task's whole tag set.",
								"items":       map[string]any{"type": "string"},
							},
							"project": stringProp("Project UUID for create, edit, or move-to-project."),
							"area":    stringProp("Area UUID for create, edit, or move-to-area."),
							"heading": stringProp("Heading UUID for create or edit."),
							"type":    enumProp("Item type for create.", []string{"task", "project", "heading"}),
						},
						"required":             []string{"cmd"},
						"additionalProperties": false,
					},
				},
				"dry_run": dryRunProp(),
			}, []string{"operations"}),
		},
		{
			Name:        "trash_task",
			Title:       "Trash Task",
			Description: "Move a task to trash.",
			InputSchema: objectSchema(map[string]any{
				"uuid":    stringProp("Task UUID."),
				"dry_run": dryRunProp(),
			}, []string{"uuid"}),
		},
		{
			Name:        "purge_task",
			Title:       "Purge Task",
			Description: "PERMANENTLY delete a task by writing a tombstone. Unlike trash_task this is unrecoverable: the task cannot be restored from Things' Trash afterwards. Hosts must require explicit human confirmation before calling this without dry_run.",
			InputSchema: objectSchema(map[string]any{
				"uuid":    stringProp("UUID of the task to permanently delete."),
				"dry_run": dryRunProp(),
			}, []string{"uuid"}),
		},
		{
			Name:        "move_task",
			Title:       "Move Task",
			Description: "Move a task to a project, an area, or the inbox. Optionally place it under a heading when moving to a project.",
			InputSchema: objectSchema(map[string]any{
				"uuid":    stringProp("Task UUID."),
				"project": stringProp("Destination project UUID."),
				"area":    stringProp("Destination area UUID."),
				"inbox":   map[string]any{"type": "boolean", "description": "Move the task to the inbox."},
				"heading": stringProp("Heading UUID inside the destination project."),
				"dry_run": dryRunProp(),
			}, []string{"uuid"}),
		},
		{
			Name:        "move_task_to_today",
			Title:       "Move Task To Today",
			Description: "Schedule a task for Today.",
			InputSchema: objectSchema(map[string]any{
				"uuid":    stringProp("Task UUID."),
				"dry_run": dryRunProp(),
			}, []string{"uuid"}),
		},
		{
			Name:        "add_checklist",
			Title:       "Add Checklist Items",
			Description: "Add checklist items to a task.",
			InputSchema: objectSchema(map[string]any{
				"uuid": stringProp("Task UUID."),
				"items": map[string]any{
					"type":        "array",
					"description": "Checklist item titles.",
					"items":       map[string]any{"type": "string"},
				},
				"dry_run": dryRunProp(),
			}, []string{"uuid", "items"}),
		},
		{
			Name:        "list_projects",
			Title:       "List Projects",
			Description: "List active Things projects.",
			InputSchema: objectSchema(map[string]any{
				"limit": limitProp(),
			}, nil),
		},
		{
			Name:        "list_areas",
			Title:       "List Areas",
			Description: "List Things areas.",
			InputSchema: objectSchema(map[string]any{
				"limit": limitProp(),
			}, nil),
		},
		{
			Name:        "list_tags",
			Title:       "List Tags",
			Description: "List Things tags.",
			InputSchema: objectSchema(map[string]any{
				"limit": limitProp(),
			}, nil),
		},
	}
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func enumProp(description string, values []string) map[string]any {
	return map[string]any{"type": "string", "description": description, "enum": values}
}

func dryRunProp() map[string]any {
	return map[string]any{"type": "boolean", "description": "Build and return the payload without writing to Things Cloud."}
}

func limitProp() map[string]any {
	return map[string]any{
		"type":        "integer",
		"description": "Maximum number of entries to return.",
		"minimum":     1,
		"maximum":     200,
	}
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *mcpServer) callTool(raw json.RawMessage) (toolResult, error) {
	var params toolCallParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return toolResult{}, fmt.Errorf("invalid tool call params: %w", err)
	}

	switch params.Name {
	case "list_tasks":
		var args struct {
			View  string `json:"view"`
			Limit int    `json:"limit"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		return s.listTasks(args.View, "", args.Limit)
	case "search_tasks":
		var args struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		if strings.TrimSpace(args.Query) == "" {
			return toolResult{}, fmt.Errorf("query is required")
		}
		return s.listTasks("all", args.Query, args.Limit)
	case "create_task":
		var args createTaskArgs
		if err := decodeArgsStrict(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		return s.createTask(args)
	case "create_project":
		var args struct {
			Title  string `json:"title"`
			Note   string `json:"note"`
			Area   string `json:"area"`
			DryRun bool   `json:"dry_run"`
		}
		if err := decodeArgsStrict(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		return s.createProject(args.Title, args.Note, args.Area, args.DryRun)
	case "create_heading":
		var args struct {
			Title   string `json:"title"`
			Project string `json:"project"`
			DryRun  bool   `json:"dry_run"`
		}
		if err := decodeArgsStrict(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		return s.createHeading(args.Title, args.Project, args.DryRun)
	case "create_area":
		var args struct {
			Title  string   `json:"title"`
			Tags   []string `json:"tags"`
			DryRun bool     `json:"dry_run"`
		}
		if err := decodeArgsStrict(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		return s.createArea(args.Title, args.Tags, args.DryRun)
	case "create_tag":
		var args struct {
			Title     string `json:"title"`
			Shorthand string `json:"shorthand"`
			Parent    string `json:"parent"`
			DryRun    bool   `json:"dry_run"`
		}
		if err := decodeArgsStrict(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		return s.createTag(args.Title, args.Shorthand, args.Parent, args.DryRun)
	case "complete_task":
		var args struct {
			UUID   string `json:"uuid"`
			DryRun bool   `json:"dry_run"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		return s.completeTask(args.UUID, args.DryRun)
	case "edit_task":
		var args editTaskArgs
		if err := decodeArgsStrict(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		return s.editTask(args)
	case "batch_tasks":
		var args struct {
			Operations []batchTaskOp `json:"operations"`
			DryRun     bool          `json:"dry_run"`
		}
		if err := decodeArgsStrict(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		return s.batchTasks(args.Operations, args.DryRun)
	case "trash_task":
		var args struct {
			UUID   string `json:"uuid"`
			DryRun bool   `json:"dry_run"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		return s.trashTask(args.UUID, args.DryRun)
	case "purge_task":
		var args struct {
			UUID   string `json:"uuid"`
			DryRun bool   `json:"dry_run"`
		}
		if err := decodeArgsStrict(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		return s.purgeTask(args.UUID, args.DryRun)
	case "move_task":
		var args struct {
			UUID    string `json:"uuid"`
			Project string `json:"project"`
			Area    string `json:"area"`
			Inbox   bool   `json:"inbox"`
			Heading string `json:"heading"`
			DryRun  bool   `json:"dry_run"`
		}
		if err := decodeArgsStrict(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		return s.moveTask(args.UUID, args.Project, args.Area, args.Heading, args.Inbox, args.DryRun)
	case "move_task_to_today":
		var args struct {
			UUID   string `json:"uuid"`
			DryRun bool   `json:"dry_run"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		return s.moveTaskToToday(args.UUID, args.DryRun)
	case "add_checklist":
		var args struct {
			UUID   string   `json:"uuid"`
			Items  []string `json:"items"`
			DryRun bool     `json:"dry_run"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		return s.addChecklist(args.UUID, args.Items, args.DryRun)
	case "list_projects":
		var args struct {
			Limit int `json:"limit"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		return s.listProjects(args.Limit)
	case "list_areas":
		var args struct {
			Limit int `json:"limit"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		return s.listAreas(args.Limit)
	case "list_tags":
		var args struct {
			Limit int `json:"limit"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		return s.listTags(args.Limit)
	default:
		return toolResult{}, fmt.Errorf("unknown tool: %s", params.Name)
	}
}

func decodeArgs(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// decodeArgsStrict rejects unknown fields so unsupported batch operation
// fields fail loudly instead of being silently dropped.
func decodeArgsStrict(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func (s *mcpServer) ensureCloud() error {
	if s.client != nil && s.history != nil {
		return nil
	}
	username := os.Getenv("THINGS_USERNAME")
	password := os.Getenv("THINGS_PASSWORD")
	if username == "" || password == "" {
		return fmt.Errorf("THINGS_USERNAME and THINGS_PASSWORD are required")
	}
	endpoint := s.endpoint
	if endpoint == "" {
		endpoint = os.Getenv("THINGS_ENDPOINT") // point the server at a test endpoint
	}
	if endpoint == "" {
		endpoint = thingscloud.APIEndpoint
	}
	client := thingscloud.New(endpoint, username, password)
	if os.Getenv("THINGS_DEBUG") != "" {
		client.Debug = true
	}
	// OwnHistory calls Verify internally, so a separate Verify call here
	// would send a second account request on every cold start.
	history, err := client.OwnHistory()
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	s.client = client
	s.history = history
	return nil
}

type simpleTask struct {
	UUID          string  `json:"uuid"`
	Title         string  `json:"title"`
	Status        string  `json:"status"`
	View          string  `json:"view,omitempty"`
	ScheduledDate *string `json:"scheduledDate,omitempty"`
	DeadlineDate  *string `json:"deadlineDate,omitempty"`
}

func (s *mcpServer) listTasks(view, query string, limit int) (toolResult, error) {
	state, err := s.loadState()
	if err != nil {
		return toolError(err), nil
	}
	if view == "" {
		view = "all"
	}
	query = strings.ToLower(strings.TrimSpace(query))
	now := time.Now().UTC()
	tomorrowStart := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)

	tasks := []simpleTask{}
	for _, task := range state.Tasks {
		if task.InTrash || task.Status == thingscloud.TaskStatusCompleted || task.Type == thingscloud.TaskTypeProject {
			continue
		}
		if !matchesView(task, view, now, tomorrowStart) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(task.Title), query) && !strings.Contains(strings.ToLower(task.Note), query) {
			continue
		}
		tasks = append(tasks, toSimpleTask(task, now, tomorrowStart))
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].ScheduledDate != nil && tasks[j].ScheduledDate != nil && *tasks[i].ScheduledDate != *tasks[j].ScheduledDate {
			return *tasks[i].ScheduledDate < *tasks[j].ScheduledDate
		}
		if tasks[i].Title != tasks[j].Title {
			return tasks[i].Title < tasks[j].Title
		}
		return tasks[i].UUID < tasks[j].UUID
	})
	if limit > 0 && limit < len(tasks) {
		tasks = tasks[:limit]
	}
	return toolJSON(tasks), nil
}

type simpleProject struct {
	UUID   string `json:"uuid"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

type simpleArea struct {
	UUID  string `json:"uuid"`
	Title string `json:"title"`
}

type simpleTag struct {
	UUID      string   `json:"uuid"`
	Title     string   `json:"title"`
	Shorthand string   `json:"shorthand,omitempty"`
	ParentIDs []string `json:"parentIds,omitempty"`
}

func (s *mcpServer) listProjects(limit int) (toolResult, error) {
	state, err := s.loadState()
	if err != nil {
		return toolError(err), nil
	}
	projects := []simpleProject{}
	for _, task := range state.Tasks {
		if task.Type != thingscloud.TaskTypeProject || task.InTrash || task.Status == thingscloud.TaskStatusCompleted {
			continue
		}
		projects = append(projects, simpleProject{UUID: task.UUID, Title: task.Title, Status: taskStatus(task)})
	}
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Title != projects[j].Title {
			return projects[i].Title < projects[j].Title
		}
		return projects[i].UUID < projects[j].UUID
	})
	if limit > 0 && limit < len(projects) {
		projects = projects[:limit]
	}
	return toolJSON(projects), nil
}

func (s *mcpServer) listAreas(limit int) (toolResult, error) {
	state, err := s.loadState()
	if err != nil {
		return toolError(err), nil
	}
	areas := []simpleArea{}
	for _, area := range state.Areas {
		areas = append(areas, simpleArea{UUID: area.UUID, Title: area.Title})
	}
	sort.Slice(areas, func(i, j int) bool {
		if areas[i].Title != areas[j].Title {
			return areas[i].Title < areas[j].Title
		}
		return areas[i].UUID < areas[j].UUID
	})
	if limit > 0 && limit < len(areas) {
		areas = areas[:limit]
	}
	return toolJSON(areas), nil
}

func (s *mcpServer) listTags(limit int) (toolResult, error) {
	state, err := s.loadState()
	if err != nil {
		return toolError(err), nil
	}
	tags := []simpleTag{}
	for _, tag := range state.Tags {
		tags = append(tags, simpleTag{
			UUID:      tag.UUID,
			Title:     tag.Title,
			Shorthand: tag.ShortHand,
			ParentIDs: tag.ParentTagIDs,
		})
	}
	sort.Slice(tags, func(i, j int) bool {
		if tags[i].Title != tags[j].Title {
			return tags[i].Title < tags[j].Title
		}
		return tags[i].UUID < tags[j].UUID
	})
	if limit > 0 && limit < len(tags) {
		tags = tags[:limit]
	}
	return toolJSON(tags), nil
}

func matchesView(task *thingscloud.Task, view string, now, tomorrowStart time.Time) bool {
	switch view {
	case "all":
		return true
	case "today":
		return task.Schedule == thingscloud.TaskScheduleAnytime && task.ScheduledDate != nil && sameDay(*task.ScheduledDate, now)
	case "inbox":
		return task.Schedule == thingscloud.TaskScheduleInbox
	case "anytime":
		return task.Schedule == thingscloud.TaskScheduleAnytime && task.ScheduledDate == nil
	case "someday":
		return task.Schedule == thingscloud.TaskScheduleSomeday && task.ScheduledDate == nil
	case "upcoming":
		return task.Schedule == thingscloud.TaskScheduleSomeday && task.ScheduledDate != nil && !task.ScheduledDate.Before(tomorrowStart)
	default:
		return false
	}
}

func toSimpleTask(task *thingscloud.Task, now, tomorrowStart time.Time) simpleTask {
	out := simpleTask{
		UUID:   task.UUID,
		Title:  task.Title,
		Status: taskStatus(task),
		View:   taskView(task, now, tomorrowStart),
	}
	if task.ScheduledDate != nil {
		s := task.ScheduledDate.Format("2006-01-02")
		out.ScheduledDate = &s
	}
	if task.DeadlineDate != nil {
		s := task.DeadlineDate.Format("2006-01-02")
		out.DeadlineDate = &s
	}
	return out
}

func taskStatus(task *thingscloud.Task) string {
	if task.InTrash {
		return "trashed"
	}
	switch task.Status {
	case thingscloud.TaskStatusCompleted:
		return "completed"
	case thingscloud.TaskStatusCanceled:
		return "canceled"
	default:
		return "open"
	}
}

func taskView(task *thingscloud.Task, now, tomorrowStart time.Time) string {
	switch {
	case task.Schedule == thingscloud.TaskScheduleInbox:
		return "inbox"
	case task.Schedule == thingscloud.TaskScheduleAnytime && task.ScheduledDate != nil && sameDay(*task.ScheduledDate, now):
		return "today"
	case task.Schedule == thingscloud.TaskScheduleAnytime:
		return "anytime"
	case task.Schedule == thingscloud.TaskScheduleSomeday && task.ScheduledDate != nil && !task.ScheduledDate.Before(tomorrowStart):
		return "upcoming"
	case task.Schedule == thingscloud.TaskScheduleSomeday:
		return "someday"
	default:
		return "unknown"
	}
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

type createTaskArgs struct {
	Title     string   `json:"title"`
	Note      string   `json:"note"`
	When      string   `json:"when"`
	Scheduled string   `json:"scheduled"`
	Deadline  string   `json:"deadline"`
	Project   string   `json:"project"`
	Area      string   `json:"area"`
	Heading   string   `json:"heading"`
	Tags      []string `json:"tags"`
	Checklist []string `json:"checklist"`
	UUID      string   `json:"uuid"`
	DryRun    bool     `json:"dry_run"`
}

// createTask mirrors the CLI create command: every option maps onto the same
// newTaskCreatePayload builder, an optional caller uuid is validated (and
// generated otherwise), and inline checklist items are written after the
// task, one commit each.
func (s *mcpServer) createTask(args createTaskArgs) (toolResult, error) {
	title := strings.TrimSpace(args.Title)
	if title == "" {
		return toolResult{}, fmt.Errorf("title is required")
	}
	when := args.When
	if when == "" {
		when = "inbox"
	}
	if err := validateWhen(when); err != nil {
		return toolResult{}, err
	}
	for name, id := range map[string]string{"project": args.Project, "area": args.Area, "heading": args.Heading} {
		if id == "" {
			continue
		}
		if err := thingscloud.ValidateUUID(id); err != nil {
			return toolResult{}, fmt.Errorf("%s: %w", name, err)
		}
	}
	taskUUID := strings.TrimSpace(args.UUID)
	if taskUUID == "" {
		taskUUID = thingscloud.NewUUID()
	} else if err := thingscloud.ValidateUUID(taskUUID); err != nil {
		return toolResult{}, fmt.Errorf("uuid: %w", err)
	}

	opts := map[string]string{"when": when}
	if args.Note != "" {
		opts["note"] = args.Note
	}
	if args.Scheduled != "" {
		opts["scheduled"] = args.Scheduled
	}
	if args.Deadline != "" {
		opts["deadline"] = args.Deadline
	}
	if args.Project != "" {
		opts["project"] = args.Project
	}
	if args.Area != "" {
		opts["area"] = args.Area
	}
	if args.Heading != "" {
		opts["heading"] = args.Heading
	}
	if len(args.Tags) > 0 {
		tags, err := validateTagUUIDs(args.Tags)
		if err != nil {
			return toolResult{}, err
		}
		opts["tags"] = strings.Join(tags, ",")
	}

	payload := newTaskCreatePayload(title, opts)
	env := writeEnvelope{id: taskUUID, action: 0, kind: "Task6", payload: payload}
	envelopes := []thingscloud.Identifiable{env}
	envelopes = append(envelopes, buildChecklistEnvelopes(taskUUID, args.Checklist)...)

	if args.DryRun {
		out := map[string]any{"status": "dry-run", "uuid": taskUUID, "item": env}
		if len(envelopes) > 1 {
			out["checklist"] = envelopes[1:]
		}
		return toolJSON(out), nil
	}
	if err := s.write(envelopes...); err != nil {
		return toolError(err), nil
	}
	result := map[string]any{"status": "created", "uuid": taskUUID, "title": title}
	if len(envelopes) > 1 {
		result["checklist"] = len(envelopes) - 1
	}
	return toolJSON(result), nil
}

// createProject uses the CLI's existing project-create path: create with
// --type project sets tp=1 in newTaskCreatePayload, whose structural rule
// forces st=1 (projects are never inbox; st=0 on structural items crashes
// Things.app). Area and note follow the same builder options as CLI create.
func (s *mcpServer) createProject(title, note, area string, dryRun bool) (toolResult, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return toolResult{}, fmt.Errorf("title is required")
	}
	opts := map[string]string{"type": "project"}
	if note != "" {
		opts["note"] = note
	}
	if area != "" {
		if err := thingscloud.ValidateUUID(area); err != nil {
			return toolResult{}, fmt.Errorf("area: %w", err)
		}
		opts["area"] = area
	}
	projectUUID := thingscloud.NewUUID()
	payload := newTaskCreatePayload(title, opts)
	env := writeEnvelope{id: projectUUID, action: 0, kind: "Task6", payload: payload}
	if dryRun {
		return toolJSON(map[string]any{"status": "dry-run", "uuid": projectUUID, "item": env}), nil
	}
	if err := s.write(env); err != nil {
		return toolError(err), nil
	}
	return toolJSON(map[string]string{"status": "created", "uuid": projectUUID, "title": title}), nil
}

// createHeading uses the CLI's heading-create path: create with
// --type heading sets tp=2, and the structural rule forces st=1 (a heading
// with st=0 crashes Things.app). A heading only makes sense inside a
// project, so the project is required here.
func (s *mcpServer) createHeading(title, project string, dryRun bool) (toolResult, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return toolResult{}, fmt.Errorf("title is required")
	}
	if project == "" {
		return toolResult{}, fmt.Errorf("project is required")
	}
	if err := thingscloud.ValidateUUID(project); err != nil {
		return toolResult{}, fmt.Errorf("project: %w", err)
	}
	headingUUID := thingscloud.NewUUID()
	payload := newTaskCreatePayload(title, map[string]string{"type": "heading", "project": project})
	env := writeEnvelope{id: headingUUID, action: 0, kind: "Task6", payload: payload}
	if dryRun {
		return toolJSON(map[string]any{"status": "dry-run", "uuid": headingUUID, "item": env}), nil
	}
	if err := s.write(env); err != nil {
		return toolError(err), nil
	}
	return toolJSON(map[string]string{"status": "created", "uuid": headingUUID, "title": title}), nil
}

// createArea mirrors the CLI create-area command's payload exactly:
// {tt, ix: 0, tg, xx} as an Area3 create.
func (s *mcpServer) createArea(title string, tags []string, dryRun bool) (toolResult, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return toolResult{}, fmt.Errorf("title is required")
	}
	tg := []string{}
	if len(tags) > 0 {
		validated, err := validateTagUUIDs(tags)
		if err != nil {
			return toolResult{}, err
		}
		tg = validated
	}
	areaUUID := thingscloud.NewUUID()
	payload := map[string]any{
		"tt": title,
		"ix": 0,
		"tg": tg,
		"xx": defaultExtension(),
	}
	env := writeEnvelope{id: areaUUID, action: 0, kind: "Area3", payload: payload}
	if dryRun {
		return toolJSON(map[string]any{"status": "dry-run", "uuid": areaUUID, "item": env}), nil
	}
	if err := s.write(env); err != nil {
		return toolError(err), nil
	}
	return toolJSON(map[string]string{"status": "created", "uuid": areaUUID, "title": title}), nil
}

// createTag mirrors the CLI create-tag command's payload exactly, including
// the negative ix ("Things uses negative indices" per the CLI's HAR notes) —
// the positive-ix rule applies to Task6/ChecklistItem3 creates, not tags.
func (s *mcpServer) createTag(title, shorthand, parent string, dryRun bool) (toolResult, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return toolResult{}, fmt.Errorf("title is required")
	}
	var sh *string
	if shorthand != "" {
		sh = &shorthand
	}
	pn := []string{}
	if parent != "" {
		if err := thingscloud.ValidateUUID(parent); err != nil {
			return toolResult{}, fmt.Errorf("parent: %w", err)
		}
		pn = []string{parent}
	}
	tagUUID := thingscloud.NewUUID()
	payload := tagCreatePayload{
		Tt: title,
		Ix: -1237, // Things uses negative indices for tags
		Sh: sh,
		Pn: pn,
		Xx: defaultExtension(),
	}
	env := writeEnvelope{id: tagUUID, action: 0, kind: "Tag4", payload: payload}
	if dryRun {
		return toolJSON(map[string]any{"status": "dry-run", "uuid": tagUUID, "item": env}), nil
	}
	if err := s.write(env); err != nil {
		return toolError(err), nil
	}
	return toolJSON(map[string]string{"status": "created", "uuid": tagUUID, "title": title}), nil
}

func (s *mcpServer) completeTask(taskUUID string, dryRun bool) (toolResult, error) {
	taskUUID = strings.TrimSpace(taskUUID)
	if taskUUID == "" {
		return toolResult{}, fmt.Errorf("uuid is required")
	}
	if err := thingscloud.ValidateUUID(taskUUID); err != nil {
		return toolResult{}, err
	}
	u := newTaskUpdate().Status(3).StopDate(nowTs())
	env := writeEnvelope{id: taskUUID, action: 1, kind: "Task6", payload: u.build()}
	if dryRun {
		return toolJSON(map[string]any{"status": "dry-run", "uuid": taskUUID, "item": env}), nil
	}
	if err := s.write(env); err != nil {
		return toolError(err), nil
	}
	return toolJSON(map[string]string{"status": "completed", "uuid": taskUUID}), nil
}

type editTaskArgs struct {
	UUID      string   `json:"uuid"`
	Title     string   `json:"title"`
	Note      string   `json:"note"`
	When      string   `json:"when"`
	Scheduled string   `json:"scheduled"`
	Deadline  string   `json:"deadline"`
	Project   string   `json:"project"`
	Area      string   `json:"area"`
	Heading   string   `json:"heading"`
	Tags      []string `json:"tags"`
	DryRun    bool     `json:"dry_run"`
}

// editTask mirrors the CLI edit command, option for option: scheduled sets
// the dates (and the start state when no explicit when was given), attaching
// an area, project, or heading moves the task out of Inbox unless a schedule
// (when or scheduled) was explicit, and tags replace the whole tag set.
func (s *mcpServer) editTask(args editTaskArgs) (toolResult, error) {
	taskUUID := strings.TrimSpace(args.UUID)
	if taskUUID == "" {
		return toolResult{}, fmt.Errorf("uuid is required")
	}
	if err := thingscloud.ValidateUUID(taskUUID); err != nil {
		return toolResult{}, err
	}
	for name, id := range map[string]string{"project": args.Project, "area": args.Area, "heading": args.Heading} {
		if id == "" {
			continue
		}
		if err := thingscloud.ValidateUUID(id); err != nil {
			return toolResult{}, fmt.Errorf("%s: %w", name, err)
		}
	}

	u := newTaskUpdate()
	if strings.TrimSpace(args.Title) != "" {
		u.Title(strings.TrimSpace(args.Title))
	}
	if args.Note != "" {
		u.Note(args.Note)
	}
	if args.When != "" {
		if err := applyWhenUpdate(u, args.When); err != nil {
			return toolResult{}, err
		}
	}
	if args.Deadline != "" {
		if t := parseDate(args.Deadline); t != nil {
			u.Deadline(t.Unix())
		}
	}
	if args.Scheduled != "" {
		if t := parseDate(args.Scheduled); t != nil {
			ts := t.Unix()
			u.Scheduled(ts, ts)
			if args.When == "" {
				u.ScheduleDate(ts)
			}
		}
	}
	explicitSchedule := args.When != "" || args.Scheduled != ""
	if args.Area != "" {
		u.Area(args.Area)
		if !explicitSchedule {
			u.Anytime()
		}
	}
	if args.Project != "" {
		u.Project(args.Project)
		if !explicitSchedule {
			u.Anytime()
		}
	}
	if args.Heading != "" {
		u.Heading(args.Heading)
		if !explicitSchedule {
			u.Anytime()
		}
	}
	if len(args.Tags) > 0 {
		validated, err := validateTagUUIDs(args.Tags)
		if err != nil {
			return toolResult{}, err
		}
		u.Tags(validated)
	}
	if !u.changed() {
		return toolResult{}, fmt.Errorf("at least one change is required")
	}
	env := writeEnvelope{id: taskUUID, action: 1, kind: "Task6", payload: u.build()}
	if args.DryRun {
		return toolJSON(map[string]any{"status": "dry-run", "uuid": taskUUID, "item": env}), nil
	}
	if err := s.write(env); err != nil {
		return toolError(err), nil
	}
	return toolJSON(map[string]string{"status": "updated", "uuid": taskUUID}), nil
}

func (s *mcpServer) trashTask(taskUUID string, dryRun bool) (toolResult, error) {
	return s.writeTaskUpdate(taskUUID, newTaskUpdate().Trash(true), dryRun, "trashed")
}

// purgeTask mirrors the CLI purge command exactly: a Tombstone2 create whose
// dloid names the task and whose own envelope UUID is freshly generated
// (cmd/things-cli cmdPurge). The target UUID is validated here even though
// the CLI skips it, because dloid is written to the wire and an invalid
// identifier poisons the history. Purge is permanent; it is deliberately not
// a batch operation.
func (s *mcpServer) purgeTask(taskUUID string, dryRun bool) (toolResult, error) {
	taskUUID = strings.TrimSpace(taskUUID)
	if taskUUID == "" {
		return toolResult{}, fmt.Errorf("uuid is required")
	}
	if err := thingscloud.ValidateUUID(taskUUID); err != nil {
		return toolResult{}, err
	}
	tombstoneUUID := thingscloud.NewUUID()
	payload := map[string]any{
		"dloid": taskUUID,
		"dld":   nowTs(),
	}
	env := writeEnvelope{id: tombstoneUUID, action: 0, kind: "Tombstone2", payload: payload}
	if dryRun {
		return toolJSON(map[string]any{"status": "dry-run", "uuid": taskUUID, "item": env}), nil
	}
	if err := s.write(env); err != nil {
		return toolError(err), nil
	}
	return toolJSON(map[string]string{"status": "purged", "uuid": taskUUID}), nil
}

// moveTask mirrors the CLI's move conventions: move-to-project is
// Project(uuid)+Anytime, move-to-area is Area(uuid)+Anytime (batch builders
// buildBatchMoveToProject/buildBatchMoveToArea), inbox is the edit command's
// --when inbox (Inbox()), and heading is the edit command's --heading
// (Heading(uuid), the agr field). Like the CLI, a move never touches ix and
// never clears other container fields. A heading only exists inside a
// project, so it is accepted only alongside a project destination.
func (s *mcpServer) moveTask(taskUUID, project, area, heading string, inbox, dryRun bool) (toolResult, error) {
	taskUUID = strings.TrimSpace(taskUUID)
	if taskUUID == "" {
		return toolResult{}, fmt.Errorf("uuid is required")
	}
	if err := thingscloud.ValidateUUID(taskUUID); err != nil {
		return toolResult{}, err
	}
	targets := 0
	for _, set := range []bool{project != "", area != "", inbox} {
		if set {
			targets++
		}
	}
	if targets != 1 {
		return toolResult{}, fmt.Errorf("exactly one of project, area, or inbox is required")
	}
	if heading != "" && project == "" {
		return toolResult{}, fmt.Errorf("heading requires a project destination")
	}

	u := newTaskUpdate()
	result := map[string]string{"status": "moved", "uuid": taskUUID}
	switch {
	case project != "":
		if err := thingscloud.ValidateUUID(project); err != nil {
			return toolResult{}, fmt.Errorf("project: %w", err)
		}
		u.Project(project).Anytime()
		result["project"] = project
		if heading != "" {
			if err := thingscloud.ValidateUUID(heading); err != nil {
				return toolResult{}, fmt.Errorf("heading: %w", err)
			}
			u.Heading(heading)
			result["heading"] = heading
		}
	case area != "":
		if err := thingscloud.ValidateUUID(area); err != nil {
			return toolResult{}, fmt.Errorf("area: %w", err)
		}
		u.Area(area).Anytime()
		result["area"] = area
	default:
		u.Inbox()
		result["inbox"] = "true"
	}

	env := writeEnvelope{id: taskUUID, action: 1, kind: "Task6", payload: u.build()}
	if dryRun {
		return toolJSON(map[string]any{"status": "dry-run", "uuid": taskUUID, "item": env}), nil
	}
	if err := s.write(env); err != nil {
		return toolError(err), nil
	}
	return toolJSON(result), nil
}

func (s *mcpServer) moveTaskToToday(taskUUID string, dryRun bool) (toolResult, error) {
	return s.writeTaskUpdate(taskUUID, newTaskUpdate().Today(), dryRun, "moved-to-today")
}

func (s *mcpServer) writeTaskUpdate(taskUUID string, u *taskUpdate, dryRun bool, status string) (toolResult, error) {
	taskUUID = strings.TrimSpace(taskUUID)
	if taskUUID == "" {
		return toolResult{}, fmt.Errorf("uuid is required")
	}
	if err := thingscloud.ValidateUUID(taskUUID); err != nil {
		return toolResult{}, err
	}
	env := writeEnvelope{id: taskUUID, action: 1, kind: "Task6", payload: u.build()}
	if dryRun {
		return toolJSON(map[string]any{"status": "dry-run", "uuid": taskUUID, "item": env}), nil
	}
	if err := s.write(env); err != nil {
		return toolError(err), nil
	}
	return toolJSON(map[string]string{"status": status, "uuid": taskUUID}), nil
}

func (s *mcpServer) batchTasks(ops []batchTaskOp, dryRun bool) (toolResult, error) {
	if len(ops) == 0 {
		return toolResult{}, fmt.Errorf("at least one operation is required")
	}
	if len(ops) > maxBatchOperations {
		return toolResult{}, fmt.Errorf("too many operations: %d > %d", len(ops), maxBatchOperations)
	}
	envelopes, results, err := buildBatchEnvelopes(ops)
	if err != nil {
		return toolResult{}, err
	}
	if dryRun {
		return toolJSON(map[string]any{
			"status":     "dry-run",
			"operations": len(envelopes),
			"items":      envelopes,
			"results":    results,
		}), nil
	}
	if err := s.write(envelopes...); err != nil {
		return toolError(err), nil
	}
	return toolJSON(map[string]any{
		"status":     "ok",
		"operations": len(envelopes),
		"results":    results,
	}), nil
}

func (s *mcpServer) addChecklist(taskUUID string, titles []string, dryRun bool) (toolResult, error) {
	taskUUID = strings.TrimSpace(taskUUID)
	if taskUUID == "" {
		return toolResult{}, fmt.Errorf("uuid is required")
	}
	if err := thingscloud.ValidateUUID(taskUUID); err != nil {
		return toolResult{}, err
	}
	envelopes := buildChecklistEnvelopes(taskUUID, titles)
	if len(envelopes) == 0 {
		return toolResult{}, fmt.Errorf("at least one checklist item is required")
	}
	if dryRun {
		return toolJSON(map[string]any{"status": "dry-run", "uuid": taskUUID, "items": envelopes}), nil
	}
	if err := s.write(envelopes...); err != nil {
		return toolError(err), nil
	}
	return toolJSON(map[string]any{"status": "checklist-added", "uuid": taskUUID, "items": len(envelopes)}), nil
}

func (s *mcpServer) write(items ...thingscloud.Identifiable) error {
	if err := s.ensureCloud(); err != nil {
		return err
	}
	if err := s.history.Sync(); err != nil {
		return fmt.Errorf("sync history: %w", err)
	}
	// Sequential single-item commits. Multi-item map commits have twice
	// produced histories that crash real Things clients while applying the
	// pull (base58 decoder trap in the legacy sync path); one-item commits
	// have a clean record on real clients. Each Write advances
	// LatestServerIndex from the server response, so commits chain on the
	// correct ancestor-index without re-syncing between items.
	for i, item := range items {
		err := s.history.Write(item)
		if err != nil && isCommitConflict(err) {
			// 409: another writer advanced the history between our Sync and
			// this commit. Re-sync for a fresh ancestor-index and retry this
			// item exactly once; any other failure returns immediately.
			if syncErr := s.history.Sync(); syncErr != nil {
				return fmt.Errorf("re-sync after conflict on item %d/%d (%s): %w", i+1, len(items), item.UUID(), syncErr)
			}
			err = s.history.Write(item)
		}
		if err != nil {
			return fmt.Errorf("write item %d/%d (%s): %w", i+1, len(items), item.UUID(), err)
		}
	}
	return nil
}

// isCommitConflict reports whether err is History.Write's HTTP 409 error.
// The core returns fmt.Errorf("Write failed: %d", status) with no typed
// error, so this matches the formatted message.
func isCommitConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Write failed: 409")
}

func toolJSON(v any) toolResult {
	bs, err := json.Marshal(v)
	if err != nil {
		return toolError(err)
	}
	res := toolResult{Content: []textContent{{Type: "text", Text: string(bs)}}}
	// The MCP spec requires structuredContent to be a JSON object; strict
	// clients (e.g. Claude Code) reject array-valued results outright, which
	// broke every list_* tool. Only attach it for object results.
	if len(bs) > 0 && bs[0] == '{' {
		res.StructuredContent = v
	}
	return res
}

func toolError(err error) toolResult {
	return toolResult{
		Content: []textContent{{Type: "text", Text: err.Error()}},
		IsError: true,
	}
}
