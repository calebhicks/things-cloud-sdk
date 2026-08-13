package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	thingscloud "github.com/pdurlej/things-cloud-sdk"
	"github.com/pdurlej/things-cloud-sdk/internal/config"
	"github.com/pdurlej/things-cloud-sdk/internal/thingscli"
	memory "github.com/pdurlej/things-cloud-sdk/state/memory"
)

const protocolVersion = "2025-06-18"
const serverVersion = "0.2.4"
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
				"dry_run": map[string]any{
					"type":        "boolean",
					"description": "Build and return the payload without writing to Things Cloud.",
				},
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
			Description: "Edit task title, note, or schedule bucket.",
			InputSchema: objectSchema(map[string]any{
				"uuid":    stringProp("Task UUID."),
				"title":   stringProp("New task title."),
				"note":    stringProp("New task note."),
				"when":    enumProp("Schedule bucket.", []string{"inbox", "today", "anytime", "someday"}),
				"dry_run": dryRunProp(),
			}, []string{"uuid"}),
		},
		{
			Name:        "batch_tasks",
			Title:       "Batch Tasks",
			Description: "Create, edit, complete, or trash up to 50 tasks in one Things Cloud write request.",
			InputSchema: objectSchema(map[string]any{
				"operations": map[string]any{
					"type":        "array",
					"description": "Batch operations, applied in one write request.",
					"minItems":    1,
					"maxItems":    maxBatchOperations,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"cmd":       enumProp("Operation.", []string{"create", "edit", "complete", "trash"}),
							"uuid":      stringProp("Task UUID. Optional for create (generated when omitted), required otherwise."),
							"title":     stringProp("Task title. Required for create."),
							"note":      stringProp("Task note."),
							"when":      enumProp("Schedule bucket.", []string{"inbox", "today", "anytime", "someday"}),
							"scheduled": stringProp("Scheduled date as YYYY-MM-DD."),
							"deadline":  stringProp("Deadline date as YYYY-MM-DD."),
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
		var args struct {
			Title  string `json:"title"`
			Note   string `json:"note"`
			When   string `json:"when"`
			DryRun bool   `json:"dry_run"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		return s.createTask(args.Title, args.Note, args.When, args.DryRun)
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
		var args struct {
			UUID   string `json:"uuid"`
			Title  string `json:"title"`
			Note   string `json:"note"`
			When   string `json:"when"`
			DryRun bool   `json:"dry_run"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		return s.editTask(args.UUID, args.Title, args.Note, args.When, args.DryRun)
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
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.Username == "" || cfg.Password == "" {
		return fmt.Errorf("THINGS_USERNAME/THINGS_PASSWORD or config username/password are required")
	}
	endpoint := s.endpoint
	if endpoint == "" {
		endpoint = thingscloud.APIEndpoint
	}
	client := thingscloud.New(endpoint, cfg.Username, cfg.Password)
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

// mcpStateCachePath resolves the MCP state cache location with the same
// config `cache` key semantics as the CLI: an explicit config path wins, and
// the default lives next to the CLI's things-cli-state.json.
func mcpStateCachePath() string {
	cfg, err := config.Load()
	if err == nil && cfg.Cache != "" {
		return cfg.Cache
	}
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		return filepath.Join(dir, "things-cloud-sdk", "things-mcp-state.json")
	}
	return filepath.Join(os.TempDir(), "things-cloud-sdk", "things-mcp-state.json")
}

func (s *mcpServer) loadState() (*memory.State, error) {
	if err := s.ensureCloud(); err != nil {
		return nil, err
	}
	return thingscli.LoadStateWithCache(s.client, s.history, mcpStateCachePath())
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

func (s *mcpServer) createTask(title, note, when string, dryRun bool) (toolResult, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return toolResult{}, fmt.Errorf("title is required")
	}
	if when == "" {
		when = "inbox"
	}
	taskUUID := generateUUID()
	payload, err := newTaskCreatePayload(title, note, when)
	if err != nil {
		return toolResult{}, err
	}
	env := writeEnvelope{id: taskUUID, action: 0, kind: "Task6", payload: payload}
	if dryRun {
		return toolJSON(map[string]any{"status": "dry-run", "uuid": taskUUID, "item": env}), nil
	}
	if err := s.write(env); err != nil {
		return toolError(err), nil
	}
	return toolJSON(map[string]string{"status": "created", "uuid": taskUUID, "title": title}), nil
}

func (s *mcpServer) completeTask(taskUUID string, dryRun bool) (toolResult, error) {
	taskUUID = strings.TrimSpace(taskUUID)
	if taskUUID == "" {
		return toolResult{}, fmt.Errorf("uuid is required")
	}
	ts := nowTs()
	status := thingscloud.TaskStatusCompleted
	payload := map[string]any{
		"md": ts,
		"ss": status,
		"sp": ts,
	}
	env := writeEnvelope{id: taskUUID, action: 1, kind: "Task6", payload: payload}
	if dryRun {
		return toolJSON(map[string]any{"status": "dry-run", "uuid": taskUUID, "item": env}), nil
	}
	if err := s.write(env); err != nil {
		return toolError(err), nil
	}
	return toolJSON(map[string]string{"status": "completed", "uuid": taskUUID}), nil
}

func (s *mcpServer) editTask(taskUUID, title, note, when string, dryRun bool) (toolResult, error) {
	taskUUID = strings.TrimSpace(taskUUID)
	if taskUUID == "" {
		return toolResult{}, fmt.Errorf("uuid is required")
	}
	payload := map[string]any{"md": nowTs()}
	if strings.TrimSpace(title) != "" {
		payload["tt"] = strings.TrimSpace(title)
	}
	if note != "" {
		payload["nt"] = textNote(note)
	}
	if when != "" {
		if err := applyWhen(payload, when); err != nil {
			return toolResult{}, err
		}
	}
	if len(payload) == 1 {
		return toolResult{}, fmt.Errorf("at least one of title, note, or when is required")
	}
	env := writeEnvelope{id: taskUUID, action: 1, kind: "Task6", payload: payload}
	if dryRun {
		return toolJSON(map[string]any{"status": "dry-run", "uuid": taskUUID, "item": env}), nil
	}
	if err := s.write(env); err != nil {
		return toolError(err), nil
	}
	return toolJSON(map[string]string{"status": "updated", "uuid": taskUUID}), nil
}

func (s *mcpServer) trashTask(taskUUID string, dryRun bool) (toolResult, error) {
	return s.writeTaskUpdate(taskUUID, map[string]any{"md": nowTs(), "tr": true}, dryRun, "trashed")
}

func (s *mcpServer) moveTaskToToday(taskUUID string, dryRun bool) (toolResult, error) {
	payload := map[string]any{"md": nowTs()}
	if err := applyWhen(payload, "today"); err != nil {
		return toolResult{}, err
	}
	return s.writeTaskUpdate(taskUUID, payload, dryRun, "moved-to-today")
}

func (s *mcpServer) writeTaskUpdate(taskUUID string, payload map[string]any, dryRun bool, status string) (toolResult, error) {
	taskUUID = strings.TrimSpace(taskUUID)
	if taskUUID == "" {
		return toolResult{}, fmt.Errorf("uuid is required")
	}
	env := writeEnvelope{id: taskUUID, action: 1, kind: "Task6", payload: payload}
	if dryRun {
		return toolJSON(map[string]any{"status": "dry-run", "uuid": taskUUID, "item": env}), nil
	}
	if err := s.write(env); err != nil {
		return toolError(err), nil
	}
	return toolJSON(map[string]string{"status": status, "uuid": taskUUID}), nil
}

// batchTaskOp is the MCP-facing subset of thingscli.BatchOp. Decoding into
// this struct (with strict decoding) keeps the tool surface aligned with the
// documented schema; the CLI batch machinery does the actual envelope work.
type batchTaskOp struct {
	Cmd       string `json:"cmd"`
	UUID      string `json:"uuid"`
	Title     string `json:"title"`
	Note      string `json:"note"`
	When      string `json:"when"`
	Scheduled string `json:"scheduled"`
	Deadline  string `json:"deadline"`
}

func (s *mcpServer) batchTasks(ops []batchTaskOp, dryRun bool) (toolResult, error) {
	if len(ops) == 0 {
		return toolResult{}, fmt.Errorf("at least one operation is required")
	}
	cliOps := make([]thingscli.BatchOp, 0, len(ops))
	for i, op := range ops {
		switch op.Cmd {
		case "create", "edit", "complete", "trash":
		default:
			return toolResult{}, fmt.Errorf("op %d: unsupported cmd: %q", i, op.Cmd)
		}
		cliOps = append(cliOps, thingscli.BatchOp{
			Cmd:       op.Cmd,
			UUID:      strings.TrimSpace(op.UUID),
			Title:     op.Title,
			Note:      op.Note,
			When:      op.When,
			Scheduled: op.Scheduled,
			Deadline:  op.Deadline,
		})
	}
	envelopes, results, err := thingscli.BuildBatch(cliOps, maxBatchOperations)
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
	var envelopes []thingscloud.Identifiable
	now := nowTs()
	for i, title := range titles {
		title = strings.TrimSpace(title)
		if title == "" {
			continue
		}
		payload := checklistItemCreatePayload{
			Cd: now,
			Md: nil,
			Tt: title,
			Ss: 0,
			Sp: nil,
			Ix: i,
			Ts: []string{taskUUID},
			Lt: false,
			Xx: wireExtension{Sn: map[string]any{}, TypeTag: "oo"},
		}
		envelopes = append(envelopes, writeEnvelope{id: generateUUID(), action: 0, kind: "ChecklistItem3", payload: payload})
	}
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

func applyWhen(payload map[string]any, when string) error {
	switch when {
	case "inbox":
		payload["st"] = 0
		payload["sr"] = nil
		payload["tir"] = nil
	case "today":
		today := todayMidnightUTC()
		payload["st"] = 1
		payload["sr"] = today
		payload["tir"] = today
	case "anytime":
		payload["st"] = 1
		payload["sr"] = nil
		payload["tir"] = nil
	case "someday":
		payload["st"] = 2
		payload["sr"] = nil
		payload["tir"] = nil
	default:
		return fmt.Errorf("unknown when value: %s", when)
	}
	return nil
}

func (s *mcpServer) write(items ...thingscloud.Identifiable) error {
	if err := s.ensureCloud(); err != nil {
		return err
	}
	if err := s.history.Sync(); err != nil {
		return fmt.Errorf("sync history: %w", err)
	}
	return s.history.Write(items...)
}

func toolJSON(v any) toolResult {
	bs, err := json.Marshal(v)
	if err != nil {
		return toolError(err)
	}
	return toolResult{
		Content:           []textContent{{Type: "text", Text: string(bs)}},
		StructuredContent: v,
	}
}

func toolError(err error) toolResult {
	return toolResult{
		Content: []textContent{{Type: "text", Text: err.Error()}},
		IsError: true,
	}
}

type wireNote struct {
	TypeTag  string `json:"_t"`
	Checksum int64  `json:"ch"`
	Value    string `json:"v"`
	Type     int    `json:"t"`
}

type wireExtension struct {
	Sn      map[string]any `json:"sn"`
	TypeTag string         `json:"_t"`
}

type taskCreatePayload struct {
	Tp   int              `json:"tp"`
	Sr   *int64           `json:"sr"`
	Dds  *int64           `json:"dds"`
	Rt   []string         `json:"rt"`
	Rmd  *int64           `json:"rmd"`
	Ss   int              `json:"ss"`
	Tr   bool             `json:"tr"`
	Dl   []string         `json:"dl"`
	Icp  bool             `json:"icp"`
	St   int              `json:"st"`
	Ar   []string         `json:"ar"`
	Tt   string           `json:"tt"`
	Do   int              `json:"do"`
	Lai  *int64           `json:"lai"`
	Tir  *int64           `json:"tir"`
	Tg   []string         `json:"tg"`
	Agr  []string         `json:"agr"`
	Ix   int              `json:"ix"`
	Cd   float64          `json:"cd"`
	Lt   bool             `json:"lt"`
	Icc  int              `json:"icc"`
	Md   *float64         `json:"md"`
	Ti   int              `json:"ti"`
	Dd   *int64           `json:"dd"`
	Ato  *int             `json:"ato"`
	Nt   wireNote         `json:"nt"`
	Icsd *int64           `json:"icsd"`
	Pr   []string         `json:"pr"`
	Rp   *string          `json:"rp"`
	Acrd *int64           `json:"acrd"`
	Sp   *float64         `json:"sp"`
	Sb   int              `json:"sb"`
	Rr   *json.RawMessage `json:"rr"`
	Xx   wireExtension    `json:"xx"`
}

type checklistItemCreatePayload struct {
	Cd float64       `json:"cd"`
	Md *float64      `json:"md"`
	Tt string        `json:"tt"`
	Ss int           `json:"ss"`
	Sp *float64      `json:"sp"`
	Ix int           `json:"ix"`
	Ts []string      `json:"ts"`
	Lt bool          `json:"lt"`
	Xx wireExtension `json:"xx"`
}

type writeEnvelope struct {
	id      string
	action  int
	kind    string
	payload any
}

func (w writeEnvelope) UUID() string { return w.id }

func (w writeEnvelope) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		T int    `json:"t"`
		E string `json:"e"`
		P any    `json:"p"`
	}{w.action, w.kind, w.payload})
}

func newTaskCreatePayload(title, note, when string) (taskCreatePayload, error) {
	var st int
	var sr *int64
	var tir *int64
	switch when {
	case "inbox":
		st = 0
	case "today":
		st = 1
		today := todayMidnightUTC()
		sr = &today
		tir = &today
	case "anytime":
		st = 1
	case "someday":
		st = 2
	default:
		return taskCreatePayload{}, fmt.Errorf("unknown when value: %s", when)
	}

	return taskCreatePayload{
		Tp:   0,
		Sr:   sr,
		Dds:  nil,
		Rt:   []string{},
		Rmd:  nil,
		Ss:   0,
		Tr:   false,
		Dl:   []string{},
		Icp:  false,
		St:   st,
		Ar:   []string{},
		Tt:   title,
		Do:   0,
		Lai:  nil,
		Tir:  tir,
		Tg:   []string{},
		Agr:  []string{},
		Ix:   0,
		Cd:   nowTs(),
		Lt:   false,
		Icc:  0,
		Md:   nil,
		Ti:   0,
		Dd:   nil,
		Ato:  nil,
		Nt:   textNote(note),
		Icsd: nil,
		Pr:   []string{},
		Rp:   nil,
		Acrd: nil,
		Sp:   nil,
		Sb:   0,
		Rr:   nil,
		Xx:   wireExtension{Sn: map[string]any{}, TypeTag: "oo"},
	}, nil
}

func textNote(s string) wireNote {
	return wireNote{TypeTag: "tx", Checksum: int64(crc32.ChecksumIEEE([]byte(s))), Value: s, Type: 1}
}

func nowTs() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

func todayMidnightUTC() int64 {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix()
}

func generateUUID() string {
	u := uuid.New()
	const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	n := new(big.Int).SetBytes(u[:])
	base := big.NewInt(58)
	mod := new(big.Int)
	var encoded []byte
	for n.Sign() > 0 {
		n.DivMod(n, base, mod)
		encoded = append(encoded, alphabet[mod.Int64()])
	}
	for i, j := 0, len(encoded)-1; i < j; i, j = i+1, j-1 {
		encoded[i], encoded[j] = encoded[j], encoded[i]
	}
	return string(encoded)
}
