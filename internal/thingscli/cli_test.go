package thingscli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	thingscloud "github.com/pdurlej/things-cloud-sdk"
	memory "github.com/pdurlej/things-cloud-sdk/state/memory"
)

func TestLoadStateWithCacheFetchesOnlyNewItems(t *testing.T) {
	var itemsStartIndexes []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version/1/history/history-id":
			fmt.Fprint(w, `{"latest-server-index":1,"latest-schema-version":301}`)
		case "/version/1/history/history-id/items":
			itemsStartIndexes = append(itemsStartIndexes, r.URL.Query().Get("start-index"))
			fmt.Fprint(w, `{"items":[{"task-1":{"e":"Task6","t":0,"p":{"tt":"Alpha","tp":0,"st":1,"ss":0}}}],"current-item-index":1,"schema":301}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client := thingscloud.New(ts.URL, "test@example.com", "secret")
	cachePath := filepath.Join(t.TempDir(), "state.json")

	state, err := LoadStateWithCache(client, client.HistoryWithID("history-id"), cachePath)
	if err != nil {
		t.Fatalf("first load failed: %v", err)
	}
	if len(state.Tasks) != 1 || state.Tasks["task-1"] == nil {
		t.Fatalf("tasks = %#v, want task-1", state.Tasks)
	}
	if len(itemsStartIndexes) != 1 || itemsStartIndexes[0] != "0" {
		t.Fatalf("items requests = %v, want one request from index 0", itemsStartIndexes)
	}

	// The cursor is now persisted at the server head, so a second load must
	// not fetch items again.
	state, err = LoadStateWithCache(client, client.HistoryWithID("history-id"), cachePath)
	if err != nil {
		t.Fatalf("second load failed: %v", err)
	}
	if len(state.Tasks) != 1 {
		t.Fatalf("cached tasks = %#v, want task-1", state.Tasks)
	}
	if len(itemsStartIndexes) != 1 {
		t.Fatalf("items requests = %v, want no refetch on cached load", itemsStartIndexes)
	}
}

func TestLoadStateWithCacheResetsWhenCursorAheadOfServer(t *testing.T) {
	var itemsStartIndexes []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version/1/history/history-id":
			fmt.Fprint(w, `{"latest-server-index":1,"latest-schema-version":301}`)
		case "/version/1/history/history-id/items":
			itemsStartIndexes = append(itemsStartIndexes, r.URL.Query().Get("start-index"))
			fmt.Fprint(w, `{"items":[{"task-1":{"e":"Task6","t":0,"p":{"tt":"Alpha","tp":0,"st":1,"ss":0}}}],"current-item-index":1,"schema":301}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client := thingscloud.New(ts.URL, "test@example.com", "secret")
	cachePath := filepath.Join(t.TempDir(), "state.json")
	if err := saveCLIStateCache(cachePath, &cliStateCache{
		HistoryID:   "history-id",
		ServerIndex: 5,
		State:       memory.NewState(),
	}); err != nil {
		t.Fatalf("seed stale cache: %v", err)
	}

	state, err := LoadStateWithCache(client, client.HistoryWithID("history-id"), cachePath)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if len(itemsStartIndexes) != 1 || itemsStartIndexes[0] != "0" {
		t.Fatalf("items requests = %v, want a full replay from index 0", itemsStartIndexes)
	}
	if len(state.Tasks) != 1 || state.Tasks["task-1"] == nil {
		t.Fatalf("tasks = %#v, want task-1 after reset", state.Tasks)
	}

	cache, err := loadCLIStateCache(cachePath)
	if err != nil {
		t.Fatalf("reload cache: %v", err)
	}
	if cache.ServerIndex != 1 {
		t.Fatalf("cache server index = %d, want 1", cache.ServerIndex)
	}
}

func TestIsThingsBase58UUID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"real Things 22-char uuid", "Q9sihFX2SsvGaz6vv4J2Hf", true},
		{"real Things 21-char uuid", "79UbvpD3TF5sBdnxPAKSX", true},
		{"22 chars decoding above 2^128", "zzzzzzzzzzzzzzzzzzzzzz", false},
		{"too short even if valid alphabet", "2NEpo7TZRRrLZSi2U", false},
		{"20 valid chars rejected by length", "23456789ABCDEFGHJKLM", false},
		{"23 valid chars rejected by length", "23456789ABCDEFGHJKLMNPQ", false},
		{"forbidden zero digit", "09sihFX2SsvGaz6vv4J2Hf", false},
		{"forbidden capital O", "O9sihFX2SsvGaz6vv4J2Hf", false},
		{"forbidden capital I", "I9sihFX2SsvGaz6vv4J2Hf", false},
		{"forbidden lowercase l", "l9sihFX2SsvGaz6vv4J2Hf", false},
		{"rfc-4122 uuid", "1D002849-0B2F-42F8-B584", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isThingsBase58UUID(tc.in); got != tc.want {
				t.Fatalf("isThingsBase58UUID(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestBatchCreateRejectsUndecodableUUID(t *testing.T) {
	if _, _, err := buildBatchCreate(BatchOp{Cmd: "create", Title: "X", UUID: "zzzzzzzzzzzzzzzzzzzzzz"}); err == nil {
		t.Fatal("22-char uuid decoding above 2^128 should be rejected")
	}
	if _, _, err := buildBatchCreate(BatchOp{Cmd: "create", Title: "X", UUID: "not-a-things-uuid"}); err == nil {
		t.Fatal("non-Base58 uuid should be rejected")
	}
	env, result, err := buildBatchCreate(BatchOp{Cmd: "create", Title: "X", UUID: "Q9sihFX2SsvGaz6vv4J2Hf"})
	if err != nil {
		t.Fatalf("valid caller uuid rejected: %v", err)
	}
	if env.UUID() != "Q9sihFX2SsvGaz6vv4J2Hf" || result["uuid"] != "Q9sihFX2SsvGaz6vv4J2Hf" {
		t.Fatalf("caller uuid not preserved: env=%q result=%q", env.UUID(), result["uuid"])
	}
}

func TestBuildBatchValidatesOperations(t *testing.T) {
	if _, _, err := BuildBatch(nil, 50); err == nil {
		t.Fatal("empty batch should be rejected")
	}

	tooMany := make([]BatchOp, 3)
	for i := range tooMany {
		tooMany[i] = BatchOp{Cmd: "complete", UUID: fmt.Sprintf("task-%d", i)}
	}
	if _, _, err := BuildBatch(tooMany, 2); err == nil {
		t.Fatal("batch above maxOps should be rejected")
	}

	if _, _, err := BuildBatch([]BatchOp{
		{Cmd: "complete", UUID: "task-1"},
		{Cmd: "trash", UUID: "task-1"},
	}, 50); err == nil {
		t.Fatal("duplicate uuids in one batch should be rejected")
	}

	if _, _, err := BuildBatch([]BatchOp{{Cmd: "explode", UUID: "task-1"}}, 50); err == nil {
		t.Fatal("unknown cmd should be rejected")
	}

	envelopes, results, err := BuildBatch([]BatchOp{
		{Cmd: "create", Title: "Task one"},
		{Cmd: "complete", UUID: "task-2"},
	}, 50)
	if err != nil {
		t.Fatalf("valid batch failed: %v", err)
	}
	if len(envelopes) != 2 || len(results) != 2 {
		t.Fatalf("envelopes/results = %d/%d, want 2/2", len(envelopes), len(results))
	}
	if envelopes[0].UUID() == "" {
		t.Fatal("create without uuid should get a generated uuid")
	}
	if results[0]["uuid"] != envelopes[0].UUID() {
		t.Fatalf("result uuid %q != envelope uuid %q", results[0]["uuid"], envelopes[0].UUID())
	}
	if envelopes[1].UUID() != "task-2" {
		t.Fatalf("complete envelope uuid = %q, want task-2", envelopes[1].UUID())
	}
}

func TestSaveCLIStateCacheIsAtomicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "state.json")
	if err := saveCLIStateCache(cachePath, &cliStateCache{
		HistoryID:   "history-id",
		ServerIndex: 3,
		State:       memory.NewState(),
	}); err != nil {
		t.Fatalf("save failed: %v", err)
	}
	info, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("stat cache: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode = %v, want 0600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
	cache, err := loadCLIStateCache(cachePath)
	if err != nil {
		t.Fatalf("reload cache: %v", err)
	}
	if cache.HistoryID != "history-id" || cache.ServerIndex != 3 {
		t.Fatalf("cache = %#v, want history-id at index 3", cache)
	}
}

func requirePayloadMap(t *testing.T, env any) map[string]any {
	t.Helper()
	envelope, ok := env.(writeEnvelope)
	if !ok {
		t.Fatalf("expected writeEnvelope, got %T", env)
	}
	payload, ok := envelope.payload.(map[string]any)
	if !ok {
		t.Fatalf("expected map payload, got %T", envelope.payload)
	}
	return payload
}

func assertAnytimeSchedule(t *testing.T, payload map[string]any) {
	t.Helper()
	if payload["st"] != 1 {
		t.Fatalf("st = %v, want 1", payload["st"])
	}
	if payload["sr"] != nil {
		t.Fatalf("sr = %v, want nil", payload["sr"])
	}
	if payload["tir"] != nil {
		t.Fatalf("tir = %v, want nil", payload["tir"])
	}
}

func TestTaskUpdateAnytimeClearsScheduleDates(t *testing.T) {
	payload := newTaskUpdate().Project("project-1").Anytime().build()

	assertAnytimeSchedule(t, payload)
	if got := payload["pr"]; got == nil {
		t.Fatal("project field was not set")
	}
}

func TestHasExplicitSchedule(t *testing.T) {
	tests := []struct {
		name string
		opts map[string]string
		want bool
	}{
		{
			name: "none",
			opts: map[string]string{},
			want: false,
		},
		{
			name: "when",
			opts: map[string]string{"when": "today"},
			want: true,
		},
		{
			name: "scheduled",
			opts: map[string]string{"scheduled": "2026-05-20"},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasExplicitSchedule(tt.opts); got != tt.want {
				t.Fatalf("hasExplicitSchedule() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStripBoolFlag(t *testing.T) {
	args, found := stripBoolFlag([]string{"--dry-run", "Task", "--when", "today", "--dry-run"}, "dry-run")
	if !found {
		t.Fatal("dry-run flag was not detected")
	}
	want := []string{"Task", "--when", "today"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args = %v, want %v", args, want)
		}
	}
}

func TestIsHelpInvocation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "top-level long flag", args: []string{"--help"}, want: true},
		{name: "top-level short flag", args: []string{"-h"}, want: true},
		{name: "help command", args: []string{"help"}, want: true},
		{name: "subcommand long flag", args: []string{"list", "--help"}, want: true},
		{name: "subcommand short flag", args: []string{"create", "-h"}, want: true},
		{name: "task title help", args: []string{"create", "help"}, want: false},
		{name: "normal command", args: []string{"today"}, want: false},
		{name: "empty", args: nil, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isHelpInvocation(tt.args); got != tt.want {
				t.Fatalf("isHelpInvocation(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func TestBuildChecklistItemEnvelopes(t *testing.T) {
	envelopes := buildChecklistItemEnvelopes("task-1", []string{"First", "Second"})
	if len(envelopes) != 2 {
		t.Fatalf("len(envelopes) = %d, want 2", len(envelopes))
	}

	bs, err := json.Marshal(envelopes[0])
	if err != nil {
		t.Fatalf("marshal envelope failed: %v", err)
	}
	var wire struct {
		E string `json:"e"`
		P struct {
			Tt string   `json:"tt"`
			Ts []string `json:"ts"`
		} `json:"p"`
	}
	if err := json.Unmarshal(bs, &wire); err != nil {
		t.Fatalf("unmarshal wire failed: %v", err)
	}
	if wire.E != "ChecklistItem3" {
		t.Fatalf("kind = %q, want ChecklistItem3", wire.E)
	}
	if wire.P.Tt != "First" {
		t.Fatalf("title = %q, want First", wire.P.Tt)
	}
	if len(wire.P.Ts) != 1 || wire.P.Ts[0] != "task-1" {
		t.Fatalf("task refs = %v, want [task-1]", wire.P.Ts)
	}
}

func TestParseOutputArgs(t *testing.T) {
	args, format := parseOutputArgs([]string{"--today", "--format", "simple", "--area", "Work"})
	if format != "simple" {
		t.Fatalf("format = %q, want simple", format)
	}
	want := []string{"--today", "--area", "Work"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args = %v, want %v", args, want)
		}
	}

	args, format = parseOutputArgs([]string{"--simple", "--inbox"})
	if format != "simple" {
		t.Fatalf("format = %q, want simple", format)
	}
	want = []string{"--inbox"}
	if len(args) != len(want) || args[0] != want[0] {
		t.Fatalf("args = %v, want %v", args, want)
	}
}

func TestTasksToSimpleOutput(t *testing.T) {
	tasks := []TaskOutput{
		{UUID: "open-1", Title: "Open Task"},
		{UUID: "done-1", Title: "Done Task", Status: int(thingscloud.TaskStatusCompleted)},
		{UUID: "trash-1", Title: "Trash Task", InTrash: true},
	}

	got := tasksToSimpleOutput(tasks)
	wantStatus := []string{"open", "completed", "trashed"}
	for i := range wantStatus {
		if got[i].Status != wantStatus[i] {
			t.Fatalf("status[%d] = %q, want %q", i, got[i].Status, wantStatus[i])
		}
	}
	if got[0].UUID != "open-1" || got[0].Title != "Open Task" {
		t.Fatalf("simple task = %#v, want uuid/title from full output", got[0])
	}
}

func TestBatchMoveToProjectUsesNullScheduleDates(t *testing.T) {
	env, _, err := buildBatchMoveToProject(BatchOp{
		UUID:    "task-1",
		Project: "project-1",
	})
	if err != nil {
		t.Fatalf("buildBatchMoveToProject failed: %v", err)
	}

	payload := requirePayloadMap(t, env)
	assertAnytimeSchedule(t, payload)

	bs, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("MarshalJSON failed: %v", err)
	}
	var wire struct {
		P map[string]any `json:"p"`
	}
	if err := json.Unmarshal(bs, &wire); err != nil {
		t.Fatalf("unmarshal wire payload failed: %v", err)
	}
	if wire.P["sr"] != nil {
		t.Fatalf("wire sr = %v, want null", wire.P["sr"])
	}
	if wire.P["tir"] != nil {
		t.Fatalf("wire tir = %v, want null", wire.P["tir"])
	}
}

func TestBatchMoveToAreaUsesNullScheduleDates(t *testing.T) {
	env, _, err := buildBatchMoveToArea(BatchOp{
		UUID: "task-1",
		Area: "area-1",
	})
	if err != nil {
		t.Fatalf("buildBatchMoveToArea failed: %v", err)
	}

	assertAnytimeSchedule(t, requirePayloadMap(t, env))
}

func TestBatchEditAutoAnytimeUsesNullScheduleDates(t *testing.T) {
	tests := []struct {
		name string
		op   BatchOp
	}{
		{
			name: "project",
			op:   BatchOp{UUID: "task-1", Project: "project-1"},
		},
		{
			name: "area",
			op:   BatchOp{UUID: "task-1", Area: "area-1"},
		},
		{
			name: "heading",
			op:   BatchOp{UUID: "task-1", Heading: "heading-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, _, err := buildBatchEdit(tt.op)
			if err != nil {
				t.Fatalf("buildBatchEdit failed: %v", err)
			}
			assertAnytimeSchedule(t, requirePayloadMap(t, env))
		})
	}
}

func TestBatchEditExplicitWhenWinsOverAutoAnytime(t *testing.T) {
	env, _, err := buildBatchEdit(BatchOp{
		UUID:    "task-1",
		Project: "project-1",
		When:    "someday",
	})
	if err != nil {
		t.Fatalf("buildBatchEdit failed: %v", err)
	}

	payload := requirePayloadMap(t, env)
	if payload["st"] != 2 {
		t.Fatalf("st = %v, want 2", payload["st"])
	}
	if payload["sr"] != nil {
		t.Fatalf("sr = %v, want nil", payload["sr"])
	}
	if payload["tir"] != nil {
		t.Fatalf("tir = %v, want nil", payload["tir"])
	}
}

func TestCreatePayloadWithDailyRepeatDefaultsToScheduledToday(t *testing.T) {
	payload, err := newTaskCreatePayloadWithRepeat("Repeat task", map[string]string{"repeat": "every-day"})
	if err != nil {
		t.Fatalf("newTaskCreatePayloadWithRepeat failed: %v", err)
	}
	if payload.Rr == nil {
		t.Fatal("repeat payload was not set")
	}
	if payload.St != 1 {
		t.Fatalf("St = %d, want 1", payload.St)
	}
	if payload.Sr == nil || payload.Tir == nil {
		t.Fatalf("Sr/Tir = %v/%v, want scheduled first repeat date", payload.Sr, payload.Tir)
	}

	var repeat thingscloud.RepeaterConfiguration
	if err := json.Unmarshal(*payload.Rr, &repeat); err != nil {
		t.Fatalf("unmarshal repeat failed: %v", err)
	}
	if repeat.FrequencyUnit != thingscloud.FrequencyUnitDaily {
		t.Fatalf("FrequencyUnit = %d, want daily", repeat.FrequencyUnit)
	}
	if repeat.Type != int(thingscloud.RepeaterTypeScheduled) {
		t.Fatalf("Type = %d, want scheduled", repeat.Type)
	}
}

func TestCreatePayloadWithWeeklyAfterCompletionRepeat(t *testing.T) {
	payload, err := newTaskCreatePayloadWithRepeat("Repeat task", map[string]string{
		"repeat":       "after-completion:weekly:mon,wed",
		"repeat-start": "2026-05-20",
	})
	if err != nil {
		t.Fatalf("newTaskCreatePayloadWithRepeat failed: %v", err)
	}

	var repeat thingscloud.RepeaterConfiguration
	if err := json.Unmarshal(*payload.Rr, &repeat); err != nil {
		t.Fatalf("unmarshal repeat failed: %v", err)
	}
	if repeat.Type != int(thingscloud.RepeaterTypeAfterCompletion) {
		t.Fatalf("Type = %d, want after-completion", repeat.Type)
	}
	if repeat.FrequencyUnit != thingscloud.FrequencyUnitWeekly {
		t.Fatalf("FrequencyUnit = %d, want weekly", repeat.FrequencyUnit)
	}
	if len(repeat.DetailConfiguration) != 2 {
		t.Fatalf("len(DetailConfiguration) = %d, want 2", len(repeat.DetailConfiguration))
	}
	if repeat.FirstScheduledAt == nil || repeat.FirstScheduledAt.Format("2006-01-02") != "2026-05-20" {
		t.Fatalf("FirstScheduledAt = %v, want 2026-05-20", repeat.FirstScheduledAt)
	}
}

func TestBatchCreateSupportsRepeat(t *testing.T) {
	env, _, err := buildBatchCreate(BatchOp{
		Title:  "Repeat task",
		Repeat: "weekly:mon",
	})
	if err != nil {
		t.Fatalf("buildBatchCreate failed: %v", err)
	}

	var wire struct {
		P struct {
			Rr json.RawMessage `json:"rr"`
		} `json:"p"`
	}
	bs, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if err := json.Unmarshal(bs, &wire); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if len(wire.P.Rr) == 0 || string(wire.P.Rr) == "null" {
		t.Fatalf("repeat payload = %s, want object", wire.P.Rr)
	}
}

func TestBatchEditCanClearRepeat(t *testing.T) {
	env, _, err := buildBatchEdit(BatchOp{
		UUID:   "task-1",
		Repeat: "none",
	})
	if err != nil {
		t.Fatalf("buildBatchEdit failed: %v", err)
	}
	payload := requirePayloadMap(t, env)
	if _, ok := payload["rr"]; !ok {
		t.Fatal("rr field was not set")
	}
	bs, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var wire struct {
		P map[string]any `json:"p"`
	}
	if err := json.Unmarshal(bs, &wire); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if wire.P["rr"] != nil {
		t.Fatalf("wire rr = %v, want null", wire.P["rr"])
	}
}

func TestCommandNeedsHistoryHead(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"list", false},
		{"show", false},
		{"areas", false},
		{"projects", false},
		{"tags", false},
		{"today", false},
		{"inbox", false},
		{"anytime", false},
		{"someday", false},
		{"upcoming", false},
		{"search", false},
		{"completed", false},
		{"logbook", false},
		{"create", true},
		{"edit", true},
		{"complete", true},
		{"trash", true},
		{"batch", true},
	}

	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			if got := commandNeedsHistoryHead(tt.cmd); got != tt.want {
				t.Fatalf("commandNeedsHistoryHead(%q) = %v, want %v", tt.cmd, got, tt.want)
			}
		})
	}
}

func TestCLIStateCachePathUsesEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("THINGS_CLI_CACHE", path)

	if got := cliStateCachePath(); got != path {
		t.Fatalf("cliStateCachePath() = %q, want %q", got, path)
	}
}

func TestCLIStateCacheMissingFile(t *testing.T) {
	cache, err := loadCLIStateCache(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("loadCLIStateCache failed: %v", err)
	}
	if cache != nil {
		t.Fatalf("cache = %#v, want nil", cache)
	}
}

func TestCLIStateCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	state := memory.NewState()
	state.Tasks["task-1"] = &thingscloud.Task{
		UUID:  "task-1",
		Title: "Cached Task",
	}
	state.Areas["area-1"] = &thingscloud.Area{
		UUID:  "area-1",
		Title: "Cached Area",
	}

	cache := &cliStateCache{
		HistoryID:   "history-1",
		ServerIndex: 42,
		State:       state,
	}
	if err := saveCLIStateCache(path, cache); err != nil {
		t.Fatalf("saveCLIStateCache failed: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat cache failed: %v", err)
	}
	if info.IsDir() {
		t.Fatal("cache path is a directory")
	}

	loaded, err := loadCLIStateCache(path)
	if err != nil {
		t.Fatalf("loadCLIStateCache failed: %v", err)
	}
	if loaded.HistoryID != "history-1" {
		t.Fatalf("HistoryID = %q, want history-1", loaded.HistoryID)
	}
	if loaded.ServerIndex != 42 {
		t.Fatalf("ServerIndex = %d, want 42", loaded.ServerIndex)
	}
	if loaded.State.Tasks["task-1"].Title != "Cached Task" {
		t.Fatalf("task title = %q, want Cached Task", loaded.State.Tasks["task-1"].Title)
	}
	if loaded.State.Areas["area-1"].Title != "Cached Area" {
		t.Fatalf("area title = %q, want Cached Area", loaded.State.Areas["area-1"].Title)
	}
}

func TestCLIStateCacheNormalizesEmptyState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	cache := &cliStateCache{
		HistoryID:   "history-1",
		ServerIndex: 7,
		State:       &memory.State{},
	}
	if err := saveCLIStateCache(path, cache); err != nil {
		t.Fatalf("saveCLIStateCache failed: %v", err)
	}

	loaded, err := loadCLIStateCache(path)
	if err != nil {
		t.Fatalf("loadCLIStateCache failed: %v", err)
	}
	if loaded.State.Tasks == nil {
		t.Fatal("Tasks map was not initialized")
	}
	if loaded.State.Areas == nil {
		t.Fatal("Areas map was not initialized")
	}
	if loaded.State.Tags == nil {
		t.Fatal("Tags map was not initialized")
	}
	if loaded.State.CheckListItems == nil {
		t.Fatal("CheckListItems map was not initialized")
	}
}

func testStateForListFilters() *memory.State {
	state := memory.NewState()
	today := time.Now().UTC()
	tomorrow := today.Add(24 * time.Hour)

	state.Areas["area-1"] = &thingscloud.Area{UUID: "area-1", Title: "Work"}
	state.Tasks["project-1"] = &thingscloud.Task{
		UUID:  "project-1",
		Title: "Project Alpha",
		Type:  thingscloud.TaskTypeProject,
	}
	state.Tasks["inbox-1"] = &thingscloud.Task{
		UUID:     "inbox-1",
		Title:    "Inbox Task",
		Schedule: thingscloud.TaskScheduleInbox,
	}
	state.Tasks["today-1"] = &thingscloud.Task{
		UUID:          "today-1",
		Title:         "Today Task",
		Schedule:      thingscloud.TaskScheduleAnytime,
		ScheduledDate: &today,
	}
	state.Tasks["anytime-1"] = &thingscloud.Task{
		UUID:     "anytime-1",
		Title:    "Anytime Task",
		Schedule: thingscloud.TaskScheduleAnytime,
	}
	state.Tasks["someday-1"] = &thingscloud.Task{
		UUID:     "someday-1",
		Title:    "Someday Task",
		Schedule: thingscloud.TaskScheduleSomeday,
	}
	state.Tasks["upcoming-1"] = &thingscloud.Task{
		UUID:          "upcoming-1",
		Title:         "Upcoming Task",
		Note:          "needle in note",
		Schedule:      thingscloud.TaskScheduleSomeday,
		ScheduledDate: &tomorrow,
	}
	state.Tasks["project-task-1"] = &thingscloud.Task{
		UUID:          "project-task-1",
		Title:         "Project Task",
		Schedule:      thingscloud.TaskScheduleAnytime,
		ParentTaskIDs: []string{"project-1"},
	}
	state.Tasks["area-task-1"] = &thingscloud.Task{
		UUID:     "area-task-1",
		Title:    "Area Task",
		Schedule: thingscloud.TaskScheduleAnytime,
		AreaIDs:  []string{"area-1"},
	}
	state.Tasks["completed-1"] = &thingscloud.Task{
		UUID:     "completed-1",
		Title:    "Completed Task",
		Schedule: thingscloud.TaskScheduleAnytime,
		Status:   thingscloud.TaskStatusCompleted,
	}
	state.Tasks["trashed-1"] = &thingscloud.Task{
		UUID:     "trashed-1",
		Title:    "Trashed Task",
		Schedule: thingscloud.TaskScheduleAnytime,
		InTrash:  true,
	}
	return state
}

func outputUUIDs(tasks []TaskOutput) []string {
	uuids := make([]string, len(tasks))
	for i, task := range tasks {
		uuids[i] = task.UUID
	}
	return uuids
}

func requireUUIDs(t *testing.T, tasks []TaskOutput, want ...string) {
	t.Helper()
	got := outputUUIDs(tasks)
	if len(got) != len(want) {
		t.Fatalf("uuids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("uuids = %v, want %v", got, want)
		}
	}
}

func TestListTasksLocationFilters(t *testing.T) {
	state := testStateForListFilters()

	requireUUIDs(t, listTasks(state, map[string]string{"today": "true"}), "today-1")
	requireUUIDs(t, listTasks(state, map[string]string{"inbox": "true"}), "inbox-1")
	requireUUIDs(t, listTasks(state, map[string]string{"someday": "true"}), "someday-1")
	requireUUIDs(t, listTasks(state, map[string]string{"upcoming": "true"}), "upcoming-1")

	anytime := outputUUIDs(listTasks(state, map[string]string{"anytime": "true"}))
	wantAnytime := []string{"anytime-1", "area-task-1", "project-task-1"}
	if len(anytime) != len(wantAnytime) {
		t.Fatalf("anytime = %v, want %v", anytime, wantAnytime)
	}
	for i := range wantAnytime {
		if anytime[i] != wantAnytime[i] {
			t.Fatalf("anytime = %v, want %v", anytime, wantAnytime)
		}
	}
}

func TestListTasksSearchAndContainerFilters(t *testing.T) {
	state := testStateForListFilters()

	requireUUIDs(t, listTasks(state, map[string]string{"search": "needle"}), "upcoming-1")
	requireUUIDs(t, listTasks(state, map[string]string{"search": "project"}), "project-task-1")
	requireUUIDs(t, listTasks(state, map[string]string{"area": "Work"}), "area-task-1")
	requireUUIDs(t, listTasks(state, map[string]string{"project": "Project Alpha"}), "project-task-1")
}

func TestCompletedTasksIncludeCompletionEvidenceAndMetadata(t *testing.T) {
	state := testStateForListFilters()
	completedAt := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	modifiedAt := completedAt.Add(5 * time.Minute)
	state.Tasks["completed-1"].CompletionDate = &completedAt
	state.Tasks["completed-1"].ModificationDate = &modifiedAt
	state.Tasks["completed-1"].AreaIDs = []string{"area-1"}
	state.Tasks["completed-1"].ParentTaskIDs = []string{"project-1"}
	state.Tasks["completed-1"].TagIDs = []string{"tag-1"}

	got := completedTasks(state, map[string]string{"since": "2026-05-20T00:00:00Z"})
	if len(got) != 1 {
		t.Fatalf("len(completed) = %d, want 1: %#v", len(got), got)
	}
	task := got[0]
	if task.UUID != "completed-1" {
		t.Fatalf("UUID = %q, want completed-1", task.UUID)
	}
	if task.CompletedAt == nil || *task.CompletedAt != "2026-05-20T12:00:00Z" {
		t.Fatalf("CompletedAt = %v, want 2026-05-20T12:00:00Z", task.CompletedAt)
	}
	if len(task.AreaTitles) != 1 || task.AreaTitles[0] != "Work" {
		t.Fatalf("AreaTitles = %v, want [Work]", task.AreaTitles)
	}
	if len(task.ProjectTitles) != 1 || task.ProjectTitles[0] != "Project Alpha" {
		t.Fatalf("ProjectTitles = %v, want [Project Alpha]", task.ProjectTitles)
	}
	if len(task.TagIDs) != 1 || task.TagIDs[0] != "tag-1" {
		t.Fatalf("TagIDs = %v, want [tag-1]", task.TagIDs)
	}
}

func TestCompletedTasksSinceFiltersByCompletionTime(t *testing.T) {
	state := testStateForListFilters()
	completedAt := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	state.Tasks["completed-1"].CompletionDate = &completedAt

	got := completedTasks(state, map[string]string{"since": "2026-05-20T00:00:00Z"})
	if len(got) != 0 {
		t.Fatalf("len(completed) = %d, want 0: %#v", len(got), got)
	}
}
