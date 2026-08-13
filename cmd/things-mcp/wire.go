package main

// Wire payload helpers and the JSON state cache for the MCP server.
//
// Ported from cmd/things-cli/main.go (v0.4.0): the CLI is a self-contained
// package main, so its payload builders and state cache cannot be imported.
// This file carries the minimal subset the MCP server needs, unchanged where
// possible so the two stay easy to diff. Identifier generation and validation
// intentionally live in the core (thingscloud.NewUUID / ValidateUUID) and are
// not duplicated here.

import (
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"time"

	thingscloud "github.com/arthursoares/things-cloud-sdk"
	memory "github.com/arthursoares/things-cloud-sdk/state/memory"
)

// ---------------------------------------------------------------------------
// Wire envelope and note/extension primitives (as in cmd/things-cli)
// ---------------------------------------------------------------------------

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

type writeEnvelope struct {
	id      string
	action  int // 0 = create, 1 = modify
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

func emptyNote() wireNote {
	return wireNote{TypeTag: "tx", Checksum: 0, Value: "", Type: 1}
}

func noteChecksum(s string) int64 {
	return int64(crc32.ChecksumIEEE([]byte(s)))
}

func textNote(s string) wireNote {
	return wireNote{TypeTag: "tx", Checksum: noteChecksum(s), Value: s, Type: 1}
}

func defaultExtension() wireExtension {
	return wireExtension{Sn: map[string]any{}, TypeTag: "oo"}
}

func nowTs() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

func todayMidnightUTC() int64 {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix()
}

func parseDate(s string) *time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil
	}
	return &t
}

// ---------------------------------------------------------------------------
// Create payloads (as in cmd/things-cli)
// ---------------------------------------------------------------------------

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

// newTaskCreatePayload builds a full Task6 create payload from CLI-style
// options, exactly as cmd/things-cli does.
func newTaskCreatePayload(title string, opts map[string]string) taskCreatePayload {
	now := nowTs()

	// Defaults
	var st int
	var sr *int64
	var tir *int64
	var dd *int64
	tp := 0
	pr := []string{}
	agr := []string{}
	ar := []string{}
	tg := []string{}
	nt := emptyNote()

	// --type
	if v, ok := opts["type"]; ok {
		switch v {
		case "project":
			tp = 1
		case "heading":
			tp = 2
		}
	}

	// --when (schedule mapping per HAR)
	if v, ok := opts["when"]; ok {
		switch v {
		case "today":
			st = 1
			today := todayMidnightUTC()
			sr = &today
			tir = &today
		case "anytime":
			st = 1
		case "someday":
			st = 2
		case "inbox":
			st = 0
		}
	}

	// Projects and headings are structural — never inbox (st=0). A heading
	// with st=0 crashes Things.app; this must win over any --when value.
	if tp != 0 && st == 0 {
		st = 1
	}

	// --note
	if v, ok := opts["note"]; ok && v != "" {
		nt = textNote(v)
	}

	// --deadline
	if v, ok := opts["deadline"]; ok {
		if t := parseDate(v); t != nil {
			ts := t.Unix()
			dd = &ts
		}
	}

	// --scheduled (overrides sr/tir; sets st=1 with dates if not already set by --when)
	if v, ok := opts["scheduled"]; ok {
		if t := parseDate(v); t != nil {
			ts := t.Unix()
			sr = &ts
			tir = &ts
			if _, hasWhen := opts["when"]; !hasWhen {
				st = 1 // default to anytime+date
			}
		}
	}

	// --project
	if v, ok := opts["project"]; ok && v != "" {
		pr = []string{v}
		if st == 0 {
			st = 1
		}
	}

	// --area
	if v, ok := opts["area"]; ok && v != "" {
		ar = []string{v}
		if st == 0 {
			st = 1
		}
	}

	// --heading
	if v, ok := opts["heading"]; ok && v != "" {
		agr = []string{v}
		if st == 0 {
			st = 1
		}
	}

	// --tags
	if v, ok := opts["tags"]; ok && v != "" {
		tg = strings.Split(v, ",")
	}

	return taskCreatePayload{
		Tp:   tp,
		Sr:   sr,
		Dds:  nil,
		Rt:   []string{},
		Rmd:  nil,
		Ss:   0,
		Tr:   false,
		Dl:   []string{},
		Icp:  false,
		St:   st,
		Ar:   ar,
		Tt:   title,
		Do:   0,
		Lai:  nil,
		Tir:  tir,
		Tg:   tg,
		Agr:  agr,
		Ix:   0,
		Cd:   now,
		Lt:   false,
		Icc:  0,
		Md:   nil, // must be null for creates — Things.app crashes otherwise
		Ti:   0,
		Dd:   dd,
		Ato:  nil,
		Nt:   nt,
		Icsd: nil,
		Pr:   pr,
		Rp:   nil,
		Acrd: nil,
		Sp:   nil,
		Sb:   0,
		Rr:   nil,
		Xx:   defaultExtension(),
	}
}

// ---------------------------------------------------------------------------
// Fluent update builder — for sparse updates (as in cmd/things-cli)
// ---------------------------------------------------------------------------

type taskUpdate struct {
	fields map[string]any
}

func newTaskUpdate() *taskUpdate {
	return &taskUpdate{fields: map[string]any{
		"md": nowTs(),
	}}
}

func (u *taskUpdate) Title(s string) *taskUpdate {
	u.fields["tt"] = s
	return u
}

func (u *taskUpdate) Note(text string) *taskUpdate {
	u.fields["nt"] = textNote(text)
	return u
}

func (u *taskUpdate) Status(ss int) *taskUpdate {
	u.fields["ss"] = ss
	return u
}

func (u *taskUpdate) StopDate(ts float64) *taskUpdate {
	u.fields["sp"] = ts
	return u
}

func (u *taskUpdate) Trash(b bool) *taskUpdate {
	u.fields["tr"] = b
	return u
}

func (u *taskUpdate) Schedule(st int, sr, tir any) *taskUpdate {
	u.fields["st"] = st
	u.fields["sr"] = sr
	u.fields["tir"] = tir
	return u
}

func (u *taskUpdate) Today() *taskUpdate {
	today := todayMidnightUTC()
	return u.Schedule(1, today, today)
}

func (u *taskUpdate) Anytime() *taskUpdate {
	return u.Schedule(1, nil, nil)
}

func (u *taskUpdate) Someday() *taskUpdate {
	return u.Schedule(2, nil, nil)
}

func (u *taskUpdate) Inbox() *taskUpdate {
	return u.Schedule(0, nil, nil)
}

func (u *taskUpdate) ScheduleDate(ts int64) *taskUpdate {
	u.fields["sr"] = ts
	u.fields["tir"] = ts
	return u
}

func (u *taskUpdate) Deadline(dd int64) *taskUpdate {
	u.fields["dd"] = dd
	return u
}

func (u *taskUpdate) Scheduled(sr, tir int64) *taskUpdate {
	u.fields["sr"] = sr
	u.fields["tir"] = tir
	return u
}

func (u *taskUpdate) Area(uuid string) *taskUpdate {
	u.fields["ar"] = []string{uuid}
	return u
}

func (u *taskUpdate) Project(uuid string) *taskUpdate {
	u.fields["pr"] = []string{uuid}
	return u
}

func (u *taskUpdate) Heading(uuid string) *taskUpdate {
	u.fields["agr"] = []string{uuid}
	return u
}

func (u *taskUpdate) changed() bool {
	return len(u.fields) > 1 // "md" is always present
}

func (u *taskUpdate) build() map[string]any {
	return u.fields
}

// ---------------------------------------------------------------------------
// Batch envelope builders (MCP subset of cmd/things-cli batch)
// ---------------------------------------------------------------------------

// batchTaskOp is the MCP-facing subset of the CLI's BatchOp. Decoding into
// this struct (with strict decoding) keeps the tool surface aligned with the
// documented schema.
type batchTaskOp struct {
	Cmd       string `json:"cmd"`
	UUID      string `json:"uuid"`
	Title     string `json:"title"`
	Note      string `json:"note"`
	When      string `json:"when"`
	Scheduled string `json:"scheduled"`
	Deadline  string `json:"deadline"`
}

// buildBatchEnvelopes validates and converts batch operations into write
// envelopes. Duplicate UUIDs across the batch are rejected: with sequential
// single-item commits the core's per-commit duplicate check cannot see them,
// and two ops racing on one item in a single tool call is never intended.
func buildBatchEnvelopes(ops []batchTaskOp) ([]thingscloud.Identifiable, []map[string]string, error) {
	envelopes := make([]thingscloud.Identifiable, 0, len(ops))
	results := make([]map[string]string, 0, len(ops))
	seen := map[string]struct{}{}
	created := 0
	for i, op := range ops {
		var (
			env    thingscloud.Identifiable
			result map[string]string
			err    error
		)
		switch op.Cmd {
		case "create":
			created++
			env, result, err = buildBatchCreate(op, created)
		case "edit":
			env, result, err = buildBatchEdit(op)
		case "complete":
			env, result, err = buildBatchComplete(op)
		case "trash":
			env, result, err = buildBatchTrash(op)
		default:
			err = fmt.Errorf("unsupported cmd: %q", op.Cmd)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("op %d (%s): %w", i, op.Cmd, err)
		}
		if _, dup := seen[env.UUID()]; dup {
			return nil, nil, fmt.Errorf("duplicate uuid in batch: %s", env.UUID())
		}
		seen[env.UUID()] = struct{}{}
		envelopes = append(envelopes, env)
		results = append(results, result)
	}
	return envelopes, results, nil
}

// buildBatchCreate builds a Task6 create envelope. ix is this create's
// 1-based position among the batch's creates.
//
// The v0.4 CLI leaves ix at 0 on create. Both history-poisoning batches from
// the 2026-08-12 incident sent ix:0 for all 14 creates, and
// wbopan/things-cloud-mcp documents ix <= 0 as a deterministic crash in
// Things' legacy sync path — so every created item in an MCP batch gets a
// distinct positive ix instead.
func buildBatchCreate(op batchTaskOp, ix int) (thingscloud.Identifiable, map[string]string, error) {
	if strings.TrimSpace(op.Title) == "" {
		return nil, nil, fmt.Errorf("create requires title")
	}

	taskUUID := op.UUID
	if taskUUID == "" {
		taskUUID = thingscloud.NewUUID()
	} else if err := thingscloud.ValidateUUID(taskUUID); err != nil {
		return nil, nil, err
	}

	opts := map[string]string{}
	if op.Note != "" {
		opts["note"] = op.Note
	}
	if op.When != "" {
		if err := validateWhen(op.When); err != nil {
			return nil, nil, err
		}
		opts["when"] = op.When
	}
	if op.Scheduled != "" {
		opts["scheduled"] = op.Scheduled
	}
	if op.Deadline != "" {
		opts["deadline"] = op.Deadline
	}

	payload := newTaskCreatePayload(op.Title, opts)
	payload.Ix = ix
	env := writeEnvelope{id: taskUUID, action: 0, kind: "Task6", payload: payload}
	return env, map[string]string{"cmd": "create", "uuid": taskUUID, "title": op.Title}, nil
}

func buildBatchEdit(op batchTaskOp) (thingscloud.Identifiable, map[string]string, error) {
	if op.UUID == "" {
		return nil, nil, fmt.Errorf("edit requires uuid")
	}
	if err := thingscloud.ValidateUUID(op.UUID); err != nil {
		return nil, nil, err
	}

	u := newTaskUpdate()
	if op.Title != "" {
		u.Title(op.Title)
	}
	if op.Note != "" {
		u.Note(op.Note)
	}
	if op.When != "" {
		if err := applyWhenUpdate(u, op.When); err != nil {
			return nil, nil, err
		}
	}
	if op.Deadline != "" {
		if t := parseDate(op.Deadline); t != nil {
			u.Deadline(t.Unix())
		}
	}
	// Same convention as the CLI edit command: --scheduled sets the dates,
	// and also the start state when no explicit --when was given.
	if op.Scheduled != "" {
		if t := parseDate(op.Scheduled); t != nil {
			ts := t.Unix()
			u.Scheduled(ts, ts)
			if op.When == "" {
				u.ScheduleDate(ts)
			}
		}
	}
	if !u.changed() {
		return nil, nil, fmt.Errorf("edit requires at least one of title, note, when, scheduled, or deadline")
	}

	env := writeEnvelope{id: op.UUID, action: 1, kind: "Task6", payload: u.build()}
	return env, map[string]string{"cmd": "edit", "uuid": op.UUID}, nil
}

func buildBatchComplete(op batchTaskOp) (thingscloud.Identifiable, map[string]string, error) {
	if op.UUID == "" {
		return nil, nil, fmt.Errorf("complete requires uuid")
	}
	if err := thingscloud.ValidateUUID(op.UUID); err != nil {
		return nil, nil, err
	}
	u := newTaskUpdate().Status(3).StopDate(nowTs())
	env := writeEnvelope{id: op.UUID, action: 1, kind: "Task6", payload: u.build()}
	return env, map[string]string{"cmd": "complete", "uuid": op.UUID}, nil
}

func buildBatchTrash(op batchTaskOp) (thingscloud.Identifiable, map[string]string, error) {
	if op.UUID == "" {
		return nil, nil, fmt.Errorf("trash requires uuid")
	}
	if err := thingscloud.ValidateUUID(op.UUID); err != nil {
		return nil, nil, err
	}
	u := newTaskUpdate().Trash(true)
	env := writeEnvelope{id: op.UUID, action: 1, kind: "Task6", payload: u.build()}
	return env, map[string]string{"cmd": "trash", "uuid": op.UUID}, nil
}

func validateWhen(when string) error {
	switch when {
	case "inbox", "today", "anytime", "someday":
		return nil
	default:
		return fmt.Errorf("unknown when value: %s", when)
	}
}

func applyWhenUpdate(u *taskUpdate, when string) error {
	switch when {
	case "today":
		u.Today()
	case "anytime":
		u.Anytime()
	case "someday":
		u.Someday()
	case "inbox":
		u.Inbox()
	default:
		return fmt.Errorf("unknown when value: %s", when)
	}
	return nil
}

// ---------------------------------------------------------------------------
// JSON state cache (as in cmd/things-cli, plus an atomic save)
// ---------------------------------------------------------------------------

type stateCache struct {
	HistoryID   string        `json:"historyId"`
	ServerIndex int           `json:"serverIndex"`
	State       *memory.State `json:"state"`
}

// mcpStateCachePath mirrors the CLI's cache resolution (THINGS_CLI_CACHE):
// an explicit THINGS_MCP_CACHE path wins, and the default lives next to the
// CLI's things-cli-state.json.
func mcpStateCachePath() string {
	if path := os.Getenv("THINGS_MCP_CACHE"); path != "" {
		return path
	}
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		return filepath.Join(dir, "things-cloud-sdk", "things-mcp-state.json")
	}
	return filepath.Join(os.TempDir(), "things-cloud-sdk", "things-mcp-state.json")
}

func loadStateCache(path string) (*stateCache, error) {
	bs, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cache stateCache
	if err := json.Unmarshal(bs, &cache); err != nil {
		return nil, err
	}
	if cache.State == nil {
		cache.State = memory.NewState()
	} else {
		normalizeMemoryState(cache.State)
	}
	return &cache, nil
}

func normalizeMemoryState(state *memory.State) {
	if state.Areas == nil {
		state.Areas = map[string]*thingscloud.Area{}
	}
	if state.Tasks == nil {
		state.Tasks = map[string]*thingscloud.Task{}
	}
	if state.Tags == nil {
		state.Tags = map[string]*thingscloud.Tag{}
	}
	if state.CheckListItems == nil {
		state.CheckListItems = map[string]*thingscloud.CheckListItem{}
	}
}

// saveStateCache writes the cache atomically (temp file + rename) so a crash
// mid-write cannot leave truncated JSON behind; a corrupt cache would
// otherwise fail every subsequent load instead of self-healing by replay.
func saveStateCache(path string, cache *stateCache) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	bs, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func(err error) error {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return cleanup(err)
	}
	if _, err := tmp.Write(bs); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// loadState materializes Things state using the JSON state cache, fetching
// only history items newer than the cached cursor. The loop is the CLI's
// loadState, made error-returning: it resets to a full replay when the cached
// cursor is ahead of the server head, and saves the refreshed cache before
// returning.
func (s *mcpServer) loadState() (*memory.State, error) {
	if err := s.ensureCloud(); err != nil {
		return nil, err
	}
	cachePath := mcpStateCachePath()
	cache, err := loadStateCache(cachePath)
	if err != nil {
		return nil, fmt.Errorf("load state cache: %w", err)
	}

	state := memory.NewState()
	startIndex := 0
	if cache != nil && cache.HistoryID == s.history.ID {
		state = cache.State
		startIndex = cache.ServerIndex
	}

	head, err := s.client.History(s.history.ID)
	if err != nil {
		return nil, fmt.Errorf("get server index: %w", err)
	}
	latestServerIndex := head.LatestServerIndex
	if startIndex > latestServerIndex {
		state = memory.NewState()
		startIndex = 0
	}

	for {
		if startIndex >= latestServerIndex {
			break
		}
		s.history.LoadedServerIndex = startIndex
		items, hasMore, err := s.history.Items(thingscloud.ItemsOptions{StartIndex: startIndex})
		if err != nil {
			return nil, fmt.Errorf("fetch items: %w", err)
		}
		if err := state.Update(items...); err != nil {
			return nil, fmt.Errorf("update state: %w", err)
		}
		startIndex = s.history.LoadedServerIndex
		if !hasMore {
			break
		}
	}

	if err := saveStateCache(cachePath, &stateCache{
		HistoryID:   s.history.ID,
		ServerIndex: startIndex,
		State:       state,
	}); err != nil {
		return nil, fmt.Errorf("save state cache: %w", err)
	}

	return state, nil
}
