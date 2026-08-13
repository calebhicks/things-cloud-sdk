package thingscli

import (
	"encoding/json"
	"fmt"
	"hash/crc32"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	thingscloud "github.com/pdurlej/things-cloud-sdk"
	"github.com/pdurlej/things-cloud-sdk/internal/config"
	memory "github.com/pdurlej/things-cloud-sdk/state/memory"
)

// ---------------------------------------------------------------------------
// Wire-format types (no omitempty - Things expects all fields on creates)
// ---------------------------------------------------------------------------

// WireNote matches the Things note wire format exactly.
// Field order must be _t, ch, v, t (matching what Things.app expects).
type WireNote struct {
	TypeTag  string `json:"_t"`
	Checksum int64  `json:"ch"`
	Value    string `json:"v"`
	Type     int    `json:"t"`
}

// WireExtension is the required xx field: {sn: {}, _t: "oo"}.
type WireExtension struct {
	Sn      map[string]any `json:"sn"`
	TypeTag string         `json:"_t"`
}

// writeEnvelope is a single generic wrapper for history.Write().
// Implements Identifiable (UUID()) and json.Marshaler.
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

// TaskCreatePayload - all 34 fields, no omitempty, field order matches HAR.
type TaskCreatePayload struct {
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
	Nt   WireNote         `json:"nt"`
	Icsd *int64           `json:"icsd"`
	Pr   []string         `json:"pr"`
	Rp   *string          `json:"rp"`
	Acrd *int64           `json:"acrd"`
	Sp   *float64         `json:"sp"`
	Sb   int              `json:"sb"`
	Rr   *json.RawMessage `json:"rr"`
	Xx   WireExtension    `json:"xx"`
}

// ChecklistItemCreatePayload - all 9 fields for checklist item creation.
type ChecklistItemCreatePayload struct {
	Cd float64       `json:"cd"`
	Md *float64      `json:"md"`
	Tt string        `json:"tt"`
	Ss int           `json:"ss"`
	Sp *float64      `json:"sp"`
	Ix int           `json:"ix"`
	Ts []string      `json:"ts"`
	Lt bool          `json:"lt"`
	Xx WireExtension `json:"xx"`
}

// TagCreatePayload - all 5 fields for tag creation.
type TagCreatePayload struct {
	Tt string        `json:"tt"`
	Ix int           `json:"ix"`
	Sh *string       `json:"sh"`
	Pn []string      `json:"pn"`
	Xx WireExtension `json:"xx"`
}

// ---------------------------------------------------------------------------
// Helpers: notes, UUID, timestamps, errors
// ---------------------------------------------------------------------------

func emptyNote() WireNote {
	return WireNote{TypeTag: "tx", Checksum: 0, Value: "", Type: 1}
}

func noteChecksum(s string) int64 {
	return int64(crc32.ChecksumIEEE([]byte(s)))
}

func textNote(s string) WireNote {
	return WireNote{TypeTag: "tx", Checksum: noteChecksum(s), Value: s, Type: 1}
}

func defaultExtension() WireExtension {
	return WireExtension{Sn: map[string]any{}, TypeTag: "oo"}
}

func generateUUID() string {
	u := uuid.New()
	// Base58 alphabet (Bitcoin/Flickr): no 0, O, I, l
	const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	n := new(big.Int).SetBytes(u[:])
	base := big.NewInt(58)
	mod := new(big.Int)
	var encoded []byte
	for n.Sign() > 0 {
		n.DivMod(n, base, mod)
		encoded = append(encoded, alphabet[mod.Int64()])
	}
	// Reverse (big-endian)
	for i, j := 0, len(encoded)-1; i < j; i, j = i+1, j-1 {
		encoded[i], encoded[j] = encoded[j], encoded[i]
	}
	return string(encoded)
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

func parseDateOrTime(s string) (*time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		utc := t.UTC()
		return &utc, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		utc := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		return &utc, nil
	}
	return nil, fmt.Errorf("expected RFC3339 timestamp or YYYY-MM-DD date: %s", s)
}

func parseLimit(opts map[string]string) int {
	if opts["limit"] == "" {
		return 0
	}
	n, err := strconv.Atoi(opts["limit"])
	if err != nil || n < 1 {
		fatalf("--limit requires a positive integer")
	}
	return n
}

func int64Ptr(v int64) *int64 {
	return &v
}

func timestampPtr(t time.Time) *thingscloud.Timestamp {
	ts := thingscloud.Timestamp(t.UTC())
	return &ts
}

func parseArgs(args []string) map[string]string {
	result := make(map[string]string)
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			key := strings.TrimPrefix(args[i], "--")
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				result[key] = args[i+1]
				i++
			} else {
				result[key] = "true"
			}
		}
	}
	return result
}

func stripBoolFlag(args []string, name string) ([]string, bool) {
	flag := "--" + name
	out := make([]string, 0, len(args))
	found := false
	for _, arg := range args {
		if arg == flag {
			found = true
			continue
		}
		out = append(out, arg)
	}
	return out, found
}

func argsHaveBoolFlag(args []string, name string) bool {
	_, found := stripBoolFlag(args, name)
	return found
}

func hasExplicitSchedule(opts map[string]string) bool {
	_, hasWhen := opts["when"]
	_, hasScheduled := opts["scheduled"]
	return hasWhen || hasScheduled
}

type repeatPlan struct {
	Clear bool
	Rule  *thingscloud.RepeaterConfiguration
	First time.Time
}

func parseRepeatPlan(opts map[string]string) (*repeatPlan, error) {
	spec := strings.TrimSpace(opts["repeat"])
	if spec == "" {
		return nil, nil
	}

	start := time.Now().UTC()
	if v := opts["repeat-start"]; v != "" {
		t, err := parseDateOrTime(v)
		if err != nil {
			return nil, fmt.Errorf("--repeat-start: %w", err)
		}
		start = *t
	} else if v := opts["scheduled"]; v != "" {
		t, err := parseDateOrTime(v)
		if err != nil {
			return nil, fmt.Errorf("--scheduled: %w", err)
		}
		start = *t
	} else if opts["when"] == "today" {
		now := time.Now().UTC()
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	} else {
		start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	}

	spec = strings.ToLower(spec)
	switch spec {
	case "none", "off", "clear":
		return &repeatPlan{Clear: true}, nil
	}

	repeaterType := thingscloud.RepeaterTypeScheduled
	if strings.HasPrefix(spec, "after-completion:") {
		repeaterType = thingscloud.RepeaterTypeAfterCompletion
		spec = strings.TrimPrefix(spec, "after-completion:")
	}

	unit, details, err := parseRepeatPattern(spec, start)
	if err != nil {
		return nil, err
	}

	rule := thingscloud.RepeaterConfiguration{
		RepeatCount:         int64Ptr(0),
		FrequencyUnit:       unit,
		FrequencyAmplitude:  1,
		DetailConfiguration: details,
		LastScheduledAt:     timestampPtr(time.Date(4001, 1, 1, 0, 0, 0, 0, time.UTC)),
		Version:             4,
		Type:                int(repeaterType),
		TimeShift:           0,
		StartReference:      timestampPtr(start),
	}
	first := firstRepeatDate(unit, details, start)
	if first.IsZero() {
		first = start
	}
	rule.FirstScheduledAt = timestampPtr(first)

	return &repeatPlan{Rule: &rule, First: first}, nil
}

func firstRepeatDate(unit thingscloud.FrequencyUnit, details []thingscloud.RepeaterDetailConfiguration, start time.Time) time.Time {
	if unit == thingscloud.FrequencyUnitDaily {
		return start
	}
	if unit == thingscloud.FrequencyUnitWeekly {
		bestDelta := 7
		for _, detail := range details {
			if detail.Weekday == nil {
				continue
			}
			delta := (int(*detail.Weekday) - int(start.Weekday()) + 7) % 7
			if delta < bestDelta {
				bestDelta = delta
			}
		}
		if bestDelta < 7 {
			return start.AddDate(0, 0, bestDelta)
		}
	}
	return time.Time{}
}

func parseRepeatPattern(spec string, start time.Time) (thingscloud.FrequencyUnit, []thingscloud.RepeaterDetailConfiguration, error) {
	switch spec {
	case "daily", "every-day":
		return thingscloud.FrequencyUnitDaily, []thingscloud.RepeaterDetailConfiguration{{Day: int64Ptr(0)}}, nil
	case "weekly", "every-week":
		wd := start.Weekday()
		return thingscloud.FrequencyUnitWeekly, []thingscloud.RepeaterDetailConfiguration{{Weekday: &wd}}, nil
	}

	if strings.HasPrefix(spec, "weekly:") {
		rawDays := strings.TrimPrefix(spec, "weekly:")
		if strings.TrimSpace(rawDays) == "" {
			return 0, nil, fmt.Errorf("--repeat weekly requires at least one weekday")
		}
		var details []thingscloud.RepeaterDetailConfiguration
		for _, raw := range strings.Split(rawDays, ",") {
			wd, ok := parseWeekday(raw)
			if !ok {
				return 0, nil, fmt.Errorf("unsupported repeat weekday: %s", raw)
			}
			day := wd
			details = append(details, thingscloud.RepeaterDetailConfiguration{Weekday: &day})
		}
		return thingscloud.FrequencyUnitWeekly, details, nil
	}

	return 0, nil, fmt.Errorf("unsupported --repeat value %q; supported: every-day, daily, weekly, every-week, weekly:mon,wed, after-completion:every-day, after-completion:weekly:mon", spec)
}

func parseWeekday(s string) (time.Weekday, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "sun", "sunday":
		return time.Sunday, true
	case "mon", "monday":
		return time.Monday, true
	case "tue", "tues", "tuesday":
		return time.Tuesday, true
	case "wed", "wednesday":
		return time.Wednesday, true
	case "thu", "thur", "thurs", "thursday":
		return time.Thursday, true
	case "fri", "friday":
		return time.Friday, true
	case "sat", "saturday":
		return time.Saturday, true
	default:
		return time.Sunday, false
	}
}

func repeatRawMessage(plan *repeatPlan) (*json.RawMessage, error) {
	if plan == nil || plan.Clear {
		return nil, nil
	}
	bs, err := json.Marshal(plan.Rule)
	if err != nil {
		return nil, err
	}
	raw := json.RawMessage(bs)
	return &raw, nil
}

func fatal(op string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", op, err)
	os.Exit(1)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func requireArgs(args []string, min int, usage string) {
	if len(args) < min {
		fatalf("Usage: %s", usage)
	}
}

func commandName() string {
	if len(os.Args) == 0 {
		return "things-cli"
	}
	name := filepath.Base(os.Args[0])
	if name == "" || name == "." {
		return "things-cli"
	}
	return name
}

func isHelpInvocation(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "help", "--help", "-h":
		return true
	}
	for _, arg := range args[1:] {
		switch arg {
		case "--help", "-h":
			return true
		}
	}
	return false
}

func outputJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func outputDryRun(operation string, envelopes []thingscloud.Identifiable) {
	outputJSON(map[string]any{
		"status":    "dry-run",
		"operation": operation,
		"items":     envelopes,
	})
}

func writeOrDryRun(history *thingscloud.History, dryRun bool, operation string, envelopes ...thingscloud.Identifiable) bool {
	if dryRun {
		outputDryRun(operation, envelopes)
		return true
	}
	if err := history.Write(envelopes...); err != nil {
		fatal(operation, err)
	}
	return false
}

// ---------------------------------------------------------------------------
// Payload builders
// ---------------------------------------------------------------------------

func newTaskCreatePayload(title string, opts map[string]string) TaskCreatePayload {
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
			st = 1 // headings are structural - always "started" (anytime), never inbox
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
		// Tasks in projects are already triaged - auto-set anytime (st=1) unless --when was explicit
		if _, hasWhen := opts["when"]; !hasWhen {
			st = 1
		}
	}

	// --heading
	if v, ok := opts["heading"]; ok && v != "" {
		agr = []string{v}
		// Tasks under headings are structural - auto-set anytime (st=1) unless --when was explicit
		if _, hasWhen := opts["when"]; !hasWhen {
			st = 1
		}
	}

	// --area
	if v, ok := opts["area"]; ok && v != "" {
		ar = []string{v}
		// Tasks in areas are already triaged - auto-set anytime (st=1) unless --when was explicit
		if _, hasWhen := opts["when"]; !hasWhen {
			st = 1
		}
	}

	// --tags (comma-separated)
	if v, ok := opts["tags"]; ok && v != "" {
		tg = strings.Split(v, ",")
	}

	// --uuid handled by caller

	return TaskCreatePayload{
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
		Md:   nil, // must be null for creates - Things.app crashes otherwise
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

func newTaskCreatePayloadWithRepeat(title string, opts map[string]string) (TaskCreatePayload, error) {
	plan, err := parseRepeatPlan(opts)
	if err != nil {
		return TaskCreatePayload{}, err
	}
	if plan != nil && !plan.Clear && opts["when"] == "inbox" {
		return TaskCreatePayload{}, fmt.Errorf("--repeat cannot be combined with --when inbox")
	}
	if plan != nil && !plan.Clear && !hasExplicitSchedule(opts) {
		cloned := make(map[string]string, len(opts)+1)
		for k, v := range opts {
			cloned[k] = v
		}
		cloned["scheduled"] = plan.First.Format("2006-01-02")
		opts = cloned
	}

	payload := newTaskCreatePayload(title, opts)
	if plan != nil {
		raw, err := repeatRawMessage(plan)
		if err != nil {
			return TaskCreatePayload{}, err
		}
		payload.Rr = raw
	}
	return payload, nil
}

// ---------------------------------------------------------------------------
// Fluent update builder - for sparse updates (edit, complete, trash, etc.)
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

func (u *taskUpdate) ClearNote() *taskUpdate {
	u.fields["nt"] = emptyNote()
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
	return u.Schedule(1, ts, ts)
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

func (u *taskUpdate) Tags(uuids []string) *taskUpdate {
	u.fields["tg"] = uuids
	return u
}

func (u *taskUpdate) Repeater(rule *thingscloud.RepeaterConfiguration) *taskUpdate {
	u.fields["rr"] = rule
	return u
}

func (u *taskUpdate) build() map[string]any {
	return u.fields
}

// ---------------------------------------------------------------------------
// cliContext and state loading
// ---------------------------------------------------------------------------

type cliContext struct {
	client  *thingscloud.Client
	history *thingscloud.History
}

type cliStateCache struct {
	HistoryID   string        `json:"historyId"`
	ServerIndex int           `json:"serverIndex"`
	State       *memory.State `json:"state"`
}

func commandNeedsHistoryHead(cmd string) bool {
	switch cmd {
	case "create", "create-area", "create-tag", "add-checklist", "edit", "complete", "trash", "purge", "move-to-today", "batch":
		return true
	default:
		return false
	}
}

func cliStateCachePath() string {
	cfg, err := config.Load()
	if err == nil && cfg.Cache != "" {
		return cfg.Cache
	}
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		return filepath.Join(dir, "things-cloud-sdk", "things-cli-state.json")
	}
	return filepath.Join(os.TempDir(), "things-cloud-sdk", "things-cli-state.json")
}

func loadCLIStateCache(path string) (*cliStateCache, error) {
	bs, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cache cliStateCache
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

func saveCLIStateCache(path string, cache *cliStateCache) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	bs, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	// Write to a temporary file and rename so a crash mid-write cannot leave
	// truncated JSON behind; a corrupt cache would otherwise fail every
	// subsequent load instead of self-healing by replay.
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

func initCLI(syncHistoryHead bool) *cliContext {
	cfg, err := config.Load()
	if err != nil {
		fatal("load config", err)
	}
	if cfg.Username == "" {
		fatalf("THINGS_USERNAME or config username is required")
	}
	if cfg.Password == "" {
		fatalf("THINGS_PASSWORD or config password is required")
	}

	c := thingscloud.New(thingscloud.APIEndpoint, cfg.Username, cfg.Password)
	if os.Getenv("THINGS_DEBUG") != "" {
		c.Debug = true
	}

	if _, err := c.Verify(); err != nil {
		fatal("login", err)
	}

	history, err := c.OwnHistory()
	if err != nil {
		fatal("get history", err)
	}
	if syncHistoryHead {
		if err := history.Sync(); err != nil {
			fatal("sync history", err)
		}
	}

	return &cliContext{client: c, history: history}
}

// LoadStateWithCache materializes Things state using the JSON state cache at
// cachePath. It fetches only history items newer than the cached cursor,
// resets to a full replay when the cached cursor is ahead of the server head,
// and saves the refreshed cache before returning. Both the CLI and the MCP
// server share this loop; they differ only in the cache path they pass.
func LoadStateWithCache(client *thingscloud.Client, history *thingscloud.History, cachePath string) (*memory.State, error) {
	cache, err := loadCLIStateCache(cachePath)
	if err != nil {
		return nil, fmt.Errorf("load state cache: %w", err)
	}

	state := memory.NewState()
	startIndex := 0
	if cache != nil && cache.HistoryID == history.ID {
		state = cache.State
		startIndex = cache.ServerIndex
	}

	head, err := client.History(history.ID)
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
		history.LoadedServerIndex = startIndex
		items, hasMore, err := history.Items(thingscloud.ItemsOptions{StartIndex: startIndex})
		if err != nil {
			return nil, fmt.Errorf("fetch items: %w", err)
		}
		if err := state.Update(items...); err != nil {
			return nil, fmt.Errorf("update state: %w", err)
		}
		startIndex = history.LoadedServerIndex
		if !hasMore {
			break
		}
	}

	if err := saveCLIStateCache(cachePath, &cliStateCache{
		HistoryID:   history.ID,
		ServerIndex: startIndex,
		State:       state,
	}); err != nil {
		return nil, fmt.Errorf("save state cache: %w", err)
	}

	return state, nil
}

func (ctx *cliContext) loadState() *memory.State {
	state, err := LoadStateWithCache(ctx.client, ctx.history, cliStateCachePath())
	if err != nil {
		fatal("load state", err)
	}
	return state
}

// ---------------------------------------------------------------------------
// Read commands
// ---------------------------------------------------------------------------

// TaskOutput is a JSON-friendly task representation for CLI output.
type TaskOutput struct {
	UUID          string   `json:"uuid"`
	Title         string   `json:"title"`
	Note          string   `json:"note,omitempty"`
	Status        int      `json:"status"`
	InTrash       bool     `json:"inTrash"`
	IsProject     bool     `json:"isProject"`
	Schedule      int      `json:"schedule"`
	CompletedAt   *string  `json:"completedAt,omitempty"`
	ScheduledDate *string  `json:"scheduledDate,omitempty"`
	DeadlineDate  *string  `json:"deadlineDate,omitempty"`
	AreaIDs       []string `json:"areaIds,omitempty"`
	ParentIDs     []string `json:"parentIds,omitempty"`
}

type SimpleTaskOutput struct {
	UUID   string `json:"uuid"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

type CompletedTaskOutput struct {
	UUID          string   `json:"uuid"`
	Title         string   `json:"title"`
	Status        string   `json:"status"`
	CompletedAt   *string  `json:"completedAt,omitempty"`
	ModifiedAt    *string  `json:"modifiedAt,omitempty"`
	ScheduledDate *string  `json:"scheduledDate,omitempty"`
	DeadlineDate  *string  `json:"deadlineDate,omitempty"`
	AreaIDs       []string `json:"areaIds,omitempty"`
	AreaTitles    []string `json:"areaTitles,omitempty"`
	ParentIDs     []string `json:"parentIds,omitempty"`
	ParentTitles  []string `json:"parentTitles,omitempty"`
	ProjectIDs    []string `json:"projectIds,omitempty"`
	ProjectTitles []string `json:"projectTitles,omitempty"`
	TagIDs        []string `json:"tagIds,omitempty"`
}

type SimpleCompletedTaskOutput struct {
	UUID        string  `json:"uuid"`
	Title       string  `json:"title"`
	Status      string  `json:"status"`
	CompletedAt *string `json:"completedAt,omitempty"`
}

func taskToOutput(t *thingscloud.Task) TaskOutput {
	out := TaskOutput{
		UUID:      t.UUID,
		Title:     t.Title,
		Note:      t.Note,
		Status:    int(t.Status),
		InTrash:   t.InTrash,
		IsProject: t.Type == thingscloud.TaskTypeProject,
		Schedule:  int(t.Schedule),
		AreaIDs:   t.AreaIDs,
		ParentIDs: t.ParentTaskIDs,
	}
	if t.CompletionDate != nil {
		s := t.CompletionDate.UTC().Format(time.RFC3339)
		out.CompletedAt = &s
	}
	if t.ScheduledDate != nil {
		s := t.ScheduledDate.Format("2006-01-02")
		out.ScheduledDate = &s
	}
	if t.DeadlineDate != nil {
		s := t.DeadlineDate.Format("2006-01-02")
		out.DeadlineDate = &s
	}
	return out
}

func taskStatusString(t TaskOutput) string {
	if t.InTrash {
		return "trashed"
	}
	switch thingscloud.TaskStatus(t.Status) {
	case thingscloud.TaskStatusCompleted:
		return "completed"
	case thingscloud.TaskStatusCanceled:
		return "canceled"
	default:
		return "open"
	}
}

func taskToSimpleOutput(t TaskOutput) SimpleTaskOutput {
	return SimpleTaskOutput{
		UUID:   t.UUID,
		Title:  t.Title,
		Status: taskStatusString(t),
	}
}

func tasksToSimpleOutput(tasks []TaskOutput) []SimpleTaskOutput {
	out := make([]SimpleTaskOutput, len(tasks))
	for i, task := range tasks {
		out[i] = taskToSimpleOutput(task)
	}
	return out
}

func outputTasks(tasks []TaskOutput, format string) {
	if format == "simple" {
		outputJSON(tasksToSimpleOutput(tasks))
		return
	}
	outputJSON(tasks)
}

func outputTask(task TaskOutput, format string) {
	if format == "simple" {
		outputJSON(taskToSimpleOutput(task))
		return
	}
	outputJSON(task)
}

func completedTaskToOutput(state *memory.State, t *thingscloud.Task) CompletedTaskOutput {
	out := CompletedTaskOutput{
		UUID:      t.UUID,
		Title:     t.Title,
		Status:    taskStatusString(taskToOutput(t)),
		AreaIDs:   t.AreaIDs,
		ParentIDs: t.ParentTaskIDs,
		TagIDs:    t.TagIDs,
	}
	if t.CompletionDate != nil {
		s := t.CompletionDate.UTC().Format(time.RFC3339)
		out.CompletedAt = &s
	}
	if t.ModificationDate != nil {
		s := t.ModificationDate.UTC().Format(time.RFC3339)
		out.ModifiedAt = &s
	}
	if t.ScheduledDate != nil {
		s := t.ScheduledDate.Format("2006-01-02")
		out.ScheduledDate = &s
	}
	if t.DeadlineDate != nil {
		s := t.DeadlineDate.Format("2006-01-02")
		out.DeadlineDate = &s
	}
	for _, id := range t.AreaIDs {
		if area := state.Areas[id]; area != nil && area.Title != "" {
			out.AreaTitles = append(out.AreaTitles, area.Title)
		}
	}
	for _, id := range t.ParentTaskIDs {
		parent := state.Tasks[id]
		if parent == nil {
			continue
		}
		if parent.Title != "" {
			out.ParentTitles = append(out.ParentTitles, parent.Title)
		}
		if parent.Type == thingscloud.TaskTypeProject {
			out.ProjectIDs = append(out.ProjectIDs, parent.UUID)
			if parent.Title != "" {
				out.ProjectTitles = append(out.ProjectTitles, parent.Title)
			}
		}
		for _, parentID := range parent.ParentTaskIDs {
			project := state.Tasks[parentID]
			if project != nil && project.Type == thingscloud.TaskTypeProject {
				out.ProjectIDs = appendUnique(out.ProjectIDs, project.UUID)
				if project.Title != "" {
					out.ProjectTitles = appendUnique(out.ProjectTitles, project.Title)
				}
			}
		}
	}
	return out
}

func completedTaskToSimpleOutput(t CompletedTaskOutput) SimpleCompletedTaskOutput {
	return SimpleCompletedTaskOutput{
		UUID:        t.UUID,
		Title:       t.Title,
		Status:      t.Status,
		CompletedAt: t.CompletedAt,
	}
}

func outputCompletedTasks(tasks []CompletedTaskOutput, format string) {
	if format == "simple" {
		out := make([]SimpleCompletedTaskOutput, len(tasks))
		for i, task := range tasks {
			out[i] = completedTaskToSimpleOutput(task)
		}
		outputJSON(out)
		return
	}
	outputJSON(tasks)
}

func appendUnique(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func parseOutputArgs(args []string) ([]string, string) {
	format := "full"
	args, simple := stripBoolFlag(args, "simple")
	if simple {
		format = "simple"
	}

	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--format" {
			if i+1 >= len(args) {
				fatalf("--format requires full or simple")
			}
			switch args[i+1] {
			case "full", "simple":
				format = args[i+1]
			default:
				fatalf("unknown format: %s", args[i+1])
			}
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out, format
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

func listTasks(state *memory.State, opts map[string]string) []TaskOutput {
	now := time.Now().UTC()
	tomorrowStart := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	searchQuery := strings.ToLower(strings.TrimSpace(opts["search"]))

	var tasks []TaskOutput
	for _, task := range state.Tasks {
		if task.InTrash || task.Status == 3 || task.Type == thingscloud.TaskTypeProject {
			continue
		}

		// Filters
		if _, ok := opts["today"]; ok {
			if task.Schedule != thingscloud.TaskScheduleAnytime || task.ScheduledDate == nil || !sameDay(*task.ScheduledDate, now) {
				continue
			}
		}
		if _, ok := opts["inbox"]; ok {
			if task.Schedule != thingscloud.TaskScheduleInbox {
				continue
			}
		}
		if _, ok := opts["anytime"]; ok {
			if task.Schedule != thingscloud.TaskScheduleAnytime || task.ScheduledDate != nil {
				continue
			}
		}
		if _, ok := opts["someday"]; ok {
			if task.Schedule != thingscloud.TaskScheduleSomeday || task.ScheduledDate != nil {
				continue
			}
		}
		if _, ok := opts["upcoming"]; ok {
			if task.Schedule != thingscloud.TaskScheduleSomeday || task.ScheduledDate == nil || task.ScheduledDate.Before(tomorrowStart) {
				continue
			}
		}
		if searchQuery != "" {
			title := strings.ToLower(task.Title)
			note := strings.ToLower(task.Note)
			if !strings.Contains(title, searchQuery) && !strings.Contains(note, searchQuery) {
				continue
			}
		}
		if areaName, ok := opts["area"]; ok {
			areaUUID := findAreaUUID(state, areaName)
			if !containsStr(task.AreaIDs, areaUUID) {
				continue
			}
		}
		if projectName, ok := opts["project"]; ok {
			projectUUID := findProjectUUID(state, projectName)
			if !containsStr(task.ParentTaskIDs, projectUUID) {
				continue
			}
		}

		tasks = append(tasks, taskToOutput(task))
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

	return tasks
}

func completedTasks(state *memory.State, opts map[string]string) []CompletedTaskOutput {
	var since, until *time.Time
	if opts["since"] != "" {
		t, err := parseDateOrTime(opts["since"])
		if err != nil {
			fatalf("--since: %v", err)
		}
		since = t
	}
	if opts["until"] != "" {
		t, err := parseDateOrTime(opts["until"])
		if err != nil {
			fatalf("--until: %v", err)
		}
		until = t
	}
	limit := parseLimit(opts)

	var tasks []CompletedTaskOutput
	for _, task := range state.Tasks {
		if task.Type == thingscloud.TaskTypeProject || task.Status != thingscloud.TaskStatusCompleted {
			continue
		}
		if task.CompletionDate != nil {
			completedAt := task.CompletionDate.UTC()
			if since != nil && completedAt.Before(*since) {
				continue
			}
			if until != nil && !completedAt.Before(*until) {
				continue
			}
		} else if since != nil || until != nil {
			continue
		}
		tasks = append(tasks, completedTaskToOutput(state, task))
	}

	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].CompletedAt != nil && tasks[j].CompletedAt != nil && *tasks[i].CompletedAt != *tasks[j].CompletedAt {
			return *tasks[i].CompletedAt > *tasks[j].CompletedAt
		}
		if tasks[i].CompletedAt != nil && tasks[j].CompletedAt == nil {
			return true
		}
		if tasks[i].CompletedAt == nil && tasks[j].CompletedAt != nil {
			return false
		}
		if tasks[i].Title != tasks[j].Title {
			return tasks[i].Title < tasks[j].Title
		}
		return tasks[i].UUID < tasks[j].UUID
	})

	if limit > 0 && limit < len(tasks) {
		tasks = tasks[:limit]
	}
	return tasks
}

func cmdList(state *memory.State, args []string) {
	args, format := parseOutputArgs(args)
	outputTasks(listTasks(state, parseArgs(args)), format)
}

func cmdListWithOpts(state *memory.State, opts map[string]string, args []string) {
	_, format := parseOutputArgs(args)
	outputTasks(listTasks(state, opts), format)
}

func cmdSearch(state *memory.State, args []string) {
	args, format := parseOutputArgs(args)
	requireArgs(args, 1, "things-cli search <query>")
	outputTasks(listTasks(state, map[string]string{"search": strings.Join(args, " ")}), format)
}

func cmdCompleted(state *memory.State, args []string) {
	args, format := parseOutputArgs(args)
	outputCompletedTasks(completedTasks(state, parseArgs(args)), format)
}

func cmdShow(state *memory.State, uuid string, args []string) {
	_, format := parseOutputArgs(args)
	for _, task := range state.Tasks {
		if strings.HasPrefix(task.UUID, uuid) {
			outputTask(taskToOutput(task), format)
			return
		}
	}
	fatalf("task not found: %s", uuid)
}

func cmdAreas(state *memory.State) {
	type AreaOutput struct {
		UUID  string `json:"uuid"`
		Title string `json:"title"`
	}
	var areas []AreaOutput
	for _, area := range state.Areas {
		areas = append(areas, AreaOutput{UUID: area.UUID, Title: area.Title})
	}
	outputJSON(areas)
}

func cmdProjects(state *memory.State) {
	var projects []TaskOutput
	for _, task := range state.Tasks {
		if task.Type == thingscloud.TaskTypeProject && !task.InTrash && task.Status != 3 {
			projects = append(projects, taskToOutput(task))
		}
	}
	outputJSON(projects)
}

func cmdTags(state *memory.State) {
	type TagOutput struct {
		UUID      string   `json:"uuid"`
		Title     string   `json:"title"`
		Shorthand string   `json:"shorthand,omitempty"`
		ParentIDs []string `json:"parentIds,omitempty"`
	}
	var tags []TagOutput
	for _, tag := range state.Tags {
		tags = append(tags, TagOutput{
			UUID:      tag.UUID,
			Title:     tag.Title,
			Shorthand: tag.ShortHand,
			ParentIDs: tag.ParentTagIDs,
		})
	}
	outputJSON(tags)
}

// helpers for cmdList filters
func findAreaUUID(state *memory.State, name string) string {
	for _, area := range state.Areas {
		if strings.EqualFold(area.Title, name) {
			return area.UUID
		}
	}
	fatalf("area not found: %s", name)
	return ""
}

func findProjectUUID(state *memory.State, name string) string {
	for _, task := range state.Tasks {
		if task.Type == thingscloud.TaskTypeProject && strings.EqualFold(task.Title, name) {
			return task.UUID
		}
	}
	fatalf("project not found: %s", name)
	return ""
}

func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Write commands
// ---------------------------------------------------------------------------

func buildChecklistItemEnvelopes(taskUUID string, items []string) []thingscloud.Identifiable {
	now := nowTs()
	envelopes := make([]thingscloud.Identifiable, 0, len(items))
	for i, title := range items {
		itemUUID := generateUUID()
		payload := ChecklistItemCreatePayload{
			Cd: now,
			Md: nil,
			Tt: strings.TrimSpace(title),
			Ss: 0,
			Sp: nil,
			Ix: i,
			Ts: []string{taskUUID},
			Lt: false,
			Xx: defaultExtension(),
		}
		envelopes = append(envelopes, writeEnvelope{id: itemUUID, action: 0, kind: "ChecklistItem3", payload: payload})
	}
	return envelopes
}

func cmdWriteChecklistItems(history *thingscloud.History, dryRun bool, taskUUID string, items []string) {
	envelopes := buildChecklistItemEnvelopes(taskUUID, items)
	if writeOrDryRun(history, dryRun, "create checklist item", envelopes...) {
		return
	}
}

func cmdCreate(history *thingscloud.History, args []string) {
	var dryRun bool
	args, dryRun = stripBoolFlag(args, "dry-run")
	requireArgs(args, 1, "things-cli create \"Title\" [--note ...] [--when today|anytime|someday|inbox] [--deadline YYYY-MM-DD] [--scheduled YYYY-MM-DD] [--project UUID] [--heading UUID] [--area UUID] [--tags UUID,...] [--type task|project|heading] [--uuid UUID] [--checklist \"Item 1,Item 2,...\"] [--repeat SPEC] [--repeat-start YYYY-MM-DD]")

	title := args[0]
	opts := parseArgs(args[1:])

	taskUUID := opts["uuid"]
	if taskUUID == "" {
		taskUUID = generateUUID()
	}

	payload, err := newTaskCreatePayloadWithRepeat(title, opts)
	if err != nil {
		fatalf("%v", err)
	}
	env := writeEnvelope{id: taskUUID, action: 0, kind: "Task6", payload: payload}

	envelopes := []thingscloud.Identifiable{env}
	if v, ok := opts["checklist"]; ok && v != "" {
		envelopes = append(envelopes, buildChecklistItemEnvelopes(taskUUID, strings.Split(v, ","))...)
	}
	if writeOrDryRun(history, dryRun, "create task", envelopes...) {
		return
	}

	outputJSON(map[string]string{"status": "created", "uuid": taskUUID, "title": title})
}

func cmdAddChecklist(history *thingscloud.History, taskUUID string, args []string) {
	var dryRun bool
	args, dryRun = stripBoolFlag(args, "dry-run")
	requireArgs(args, 1, `things-cli add-checklist <task-uuid> "Item 1,Item 2,Item 3"`)

	items := strings.Split(args[0], ",")
	cmdWriteChecklistItems(history, dryRun, taskUUID, items)
	if dryRun {
		return
	}

	outputJSON(map[string]string{"status": "checklist-added", "uuid": taskUUID, "items": fmt.Sprintf("%d", len(items))})
}

func cmdEdit(history *thingscloud.History, taskUUID string, args []string) {
	var dryRun bool
	args, dryRun = stripBoolFlag(args, "dry-run")
	opts := parseArgs(args)
	if len(opts) == 0 {
		fatalf("Usage: things-cli edit <uuid> [--title ...] [--note ...] [--when today|anytime|someday|inbox] [--deadline YYYY-MM-DD] [--scheduled YYYY-MM-DD] [--area UUID] [--project UUID] [--heading UUID] [--tags UUID,...] [--repeat SPEC|none] [--repeat-start YYYY-MM-DD]")
	}

	u := newTaskUpdate()

	if v, ok := opts["title"]; ok {
		u.Title(v)
	}
	if v, ok := opts["note"]; ok {
		if v == "" {
			u.ClearNote()
		} else {
			u.Note(v)
		}
	}
	if v, ok := opts["when"]; ok {
		switch v {
		case "today":
			u.Today()
		case "anytime":
			u.Anytime()
		case "someday":
			u.Someday()
		case "inbox":
			u.Inbox()
		}
	}
	if v, ok := opts["deadline"]; ok {
		if t := parseDate(v); t != nil {
			u.Deadline(t.Unix())
		}
	}
	if v, ok := opts["scheduled"]; ok {
		if t := parseDate(v); t != nil {
			ts := t.Unix()
			u.Scheduled(ts, ts)
			if _, hasWhen := opts["when"]; !hasWhen {
				u.ScheduleDate(ts)
			}
		}
	}
	if v, ok := opts["area"]; ok && v != "" {
		u.Area(v)
		// When adding an area, also move out of Inbox unless a schedule was explicit.
		if !hasExplicitSchedule(opts) {
			u.Anytime()
		}
	}
	if v, ok := opts["project"]; ok && v != "" {
		u.Project(v)
		// When adding a project, also move out of Inbox unless a schedule was explicit.
		if !hasExplicitSchedule(opts) {
			u.Anytime()
		}
	}
	if v, ok := opts["heading"]; ok && v != "" {
		u.Heading(v)
		// When adding a heading, also move out of Inbox unless a schedule was explicit.
		if !hasExplicitSchedule(opts) {
			u.Anytime()
		}
	}
	if v, ok := opts["tags"]; ok && v != "" {
		u.Tags(strings.Split(v, ","))
	}
	if _, ok := opts["repeat"]; ok {
		plan, err := parseRepeatPlan(opts)
		if err != nil {
			fatalf("%v", err)
		}
		if plan != nil {
			if !plan.Clear && opts["when"] == "inbox" {
				fatalf("--repeat cannot be combined with --when inbox")
			}
			u.Repeater(plan.Rule)
			if !plan.Clear && !hasExplicitSchedule(opts) {
				u.ScheduleDate(plan.First.Unix())
			}
		}
	}

	env := writeEnvelope{id: taskUUID, action: 1, kind: "Task6", payload: u.build()}
	if writeOrDryRun(history, dryRun, "edit task", env) {
		return
	}

	outputJSON(map[string]string{"status": "updated", "uuid": taskUUID})
}

func cmdComplete(history *thingscloud.History, taskUUID string, dryRun bool) {
	ts := nowTs()
	u := newTaskUpdate().Status(3).StopDate(ts)

	env := writeEnvelope{id: taskUUID, action: 1, kind: "Task6", payload: u.build()}
	if writeOrDryRun(history, dryRun, "complete task", env) {
		return
	}

	outputJSON(map[string]string{"status": "completed", "uuid": taskUUID})
}

func cmdTrash(history *thingscloud.History, taskUUID string, dryRun bool) {
	u := newTaskUpdate().Trash(true)

	env := writeEnvelope{id: taskUUID, action: 1, kind: "Task6", payload: u.build()}
	if writeOrDryRun(history, dryRun, "trash task", env) {
		return
	}

	outputJSON(map[string]string{"status": "trashed", "uuid": taskUUID})
}

func cmdPurge(history *thingscloud.History, taskUUID string, dryRun bool) {
	tombstoneUUID := generateUUID()
	payload := map[string]any{
		"dloid": taskUUID,
		"dld":   nowTs(),
	}

	env := writeEnvelope{id: tombstoneUUID, action: 0, kind: "Tombstone2", payload: payload}
	if writeOrDryRun(history, dryRun, "purge task", env) {
		return
	}

	outputJSON(map[string]string{"status": "purged", "uuid": taskUUID})
}

func cmdMoveToToday(history *thingscloud.History, taskUUID string, dryRun bool) {
	u := newTaskUpdate().Today()

	env := writeEnvelope{id: taskUUID, action: 1, kind: "Task6", payload: u.build()}
	if writeOrDryRun(history, dryRun, "move to today", env) {
		return
	}

	outputJSON(map[string]string{"status": "moved-to-today", "uuid": taskUUID})
}

func cmdCreateArea(history *thingscloud.History, args []string) {
	var dryRun bool
	args, dryRun = stripBoolFlag(args, "dry-run")
	requireArgs(args, 1, `things-cli create-area "Name" [--tags UUID,...] [--uuid UUID]`)

	title := args[0]
	opts := parseArgs(args[1:])

	areaUUID := opts["uuid"]
	if areaUUID == "" {
		areaUUID = generateUUID()
	}

	tg := []string{}
	if v, ok := opts["tags"]; ok && v != "" {
		tg = strings.Split(v, ",")
	}

	payload := map[string]any{
		"tt": title,
		"ix": 0,
		"tg": tg,
		"xx": defaultExtension(),
	}

	env := writeEnvelope{id: areaUUID, action: 0, kind: "Area3", payload: payload}
	if writeOrDryRun(history, dryRun, "create area", env) {
		return
	}

	outputJSON(map[string]string{"status": "created", "uuid": areaUUID, "title": title})
}

func cmdCreateTag(history *thingscloud.History, args []string) {
	var dryRun bool
	args, dryRun = stripBoolFlag(args, "dry-run")
	requireArgs(args, 1, `things-cli create-tag "Name" [--shorthand KEY] [--parent UUID]`)

	title := args[0]
	opts := parseArgs(args[1:])

	tagUUID := opts["uuid"]
	if tagUUID == "" {
		tagUUID = generateUUID()
	}

	// Per HAR: ix is negative, sh is null on create, pn is []
	var sh *string
	if v, ok := opts["shorthand"]; ok {
		sh = &v
	}

	pn := []string{}
	if v, ok := opts["parent"]; ok && v != "" {
		pn = []string{v}
	}

	payload := TagCreatePayload{
		Tt: title,
		Ix: -1237, // Things uses negative indices
		Sh: sh,
		Pn: pn,
		Xx: defaultExtension(),
	}

	env := writeEnvelope{id: tagUUID, action: 0, kind: "Tag4", payload: payload}
	if writeOrDryRun(history, dryRun, "create tag", env) {
		return
	}

	outputJSON(map[string]string{"status": "created", "uuid": tagUUID, "title": title})
}

// ---------------------------------------------------------------------------
// Batch command
// ---------------------------------------------------------------------------

// BatchOp represents a single operation in a batch request.
type BatchOp struct {
	Cmd         string            `json:"cmd"`
	UUID        string            `json:"uuid,omitempty"`
	Title       string            `json:"title,omitempty"`
	Note        string            `json:"note,omitempty"`
	When        string            `json:"when,omitempty"`
	Deadline    string            `json:"deadline,omitempty"`
	Scheduled   string            `json:"scheduled,omitempty"`
	Repeat      string            `json:"repeat,omitempty"`
	RepeatStart string            `json:"repeatStart,omitempty"`
	Project     string            `json:"project,omitempty"`
	Area        string            `json:"area,omitempty"`
	Heading     string            `json:"heading,omitempty"`
	Tags        []string          `json:"tags,omitempty"`
	Type        string            `json:"type,omitempty"`
	Extra       map[string]string `json:"extra,omitempty"` // for any additional opts
}

func cmdBatch(history *thingscloud.History, dryRun bool) {
	// Read JSON from stdin
	var ops []BatchOp
	if err := json.NewDecoder(os.Stdin).Decode(&ops); err != nil {
		fatalf("parsing batch JSON: %v", err)
	}

	if len(ops) == 0 {
		fatalf("batch: no operations provided")
	}

	envelopes, results, err := BuildBatch(ops, 0)
	if err != nil {
		fatalf("batch: %v", err)
	}

	// Send all in one request
	if dryRun {
		outputJSON(map[string]any{
			"status":     "dry-run",
			"operation":  "batch write",
			"operations": len(envelopes),
			"items":      envelopes,
			"results":    results,
		})
		return
	}
	if err := history.Write(envelopes...); err != nil {
		fatal("batch write", err)
	}

	outputJSON(map[string]any{
		"status":     "ok",
		"operations": len(envelopes),
		"results":    results,
	})
}

// BuildBatch validates batch operations and converts them into Things Cloud
// write envelopes ready to pass to History.Write in one request. A maxOps of
// 0 means unlimited. Duplicate envelope UUIDs are rejected because
// History.Write serializes the batch as a JSON map keyed by UUID, which would
// silently drop all but one of the duplicate entries.
func BuildBatch(ops []BatchOp, maxOps int) ([]thingscloud.Identifiable, []map[string]string, error) {
	if len(ops) == 0 {
		return nil, nil, fmt.Errorf("no operations provided")
	}
	if maxOps > 0 && len(ops) > maxOps {
		return nil, nil, fmt.Errorf("too many operations: %d > %d", len(ops), maxOps)
	}

	envelopes := make([]thingscloud.Identifiable, 0, len(ops))
	results := make([]map[string]string, 0, len(ops))
	seen := map[string]struct{}{}
	for i, op := range ops {
		env, result, err := buildBatchEnvelope(op)
		if err != nil {
			return nil, nil, fmt.Errorf("op %d (%s): %w", i, op.Cmd, err)
		}
		id := env.UUID()
		if _, ok := seen[id]; ok {
			return nil, nil, fmt.Errorf("duplicate uuid in batch: %s", id)
		}
		seen[id] = struct{}{}
		envelopes = append(envelopes, env)
		results = append(results, result)
	}
	return envelopes, results, nil
}

func buildBatchEnvelope(op BatchOp) (thingscloud.Identifiable, map[string]string, error) {
	switch op.Cmd {
	case "create":
		return buildBatchCreate(op)
	case "complete":
		return buildBatchComplete(op)
	case "trash":
		return buildBatchTrash(op)
	case "purge":
		return buildBatchPurge(op)
	case "move-to-today":
		return buildBatchMoveToToday(op)
	case "move-to-project":
		return buildBatchMoveToProject(op)
	case "move-to-area":
		return buildBatchMoveToArea(op)
	case "edit":
		return buildBatchEdit(op)
	default:
		return nil, nil, fmt.Errorf("unknown command: %s", op.Cmd)
	}
}

func buildBatchCreate(op BatchOp) (thingscloud.Identifiable, map[string]string, error) {
	if op.Title == "" {
		return nil, nil, fmt.Errorf("create requires title")
	}

	taskUUID := op.UUID
	if taskUUID == "" {
		taskUUID = generateUUID()
	}

	// Convert BatchOp to opts map for newTaskCreatePayload
	opts := make(map[string]string)
	if op.Note != "" {
		opts["note"] = op.Note
	}
	if op.When != "" {
		opts["when"] = op.When
	}
	if op.Deadline != "" {
		opts["deadline"] = op.Deadline
	}
	if op.Scheduled != "" {
		opts["scheduled"] = op.Scheduled
	}
	if op.Repeat != "" {
		opts["repeat"] = op.Repeat
	}
	if op.RepeatStart != "" {
		opts["repeat-start"] = op.RepeatStart
	}
	if op.Project != "" {
		opts["project"] = op.Project
	}
	if op.Area != "" {
		opts["area"] = op.Area
	}
	if op.Heading != "" {
		opts["heading"] = op.Heading
	}
	if len(op.Tags) > 0 {
		opts["tags"] = strings.Join(op.Tags, ",")
	}
	if op.Type != "" {
		opts["type"] = op.Type
	}
	for k, v := range op.Extra {
		opts[k] = v
	}

	payload, err := newTaskCreatePayloadWithRepeat(op.Title, opts)
	if err != nil {
		return nil, nil, err
	}
	env := writeEnvelope{id: taskUUID, action: 0, kind: "Task6", payload: payload}

	return env, map[string]string{"cmd": "create", "uuid": taskUUID, "title": op.Title}, nil
}

func buildBatchComplete(op BatchOp) (thingscloud.Identifiable, map[string]string, error) {
	if op.UUID == "" {
		return nil, nil, fmt.Errorf("complete requires uuid")
	}

	ts := nowTs()
	u := newTaskUpdate().Status(3).StopDate(ts)
	env := writeEnvelope{id: op.UUID, action: 1, kind: "Task6", payload: u.build()}

	return env, map[string]string{"cmd": "complete", "uuid": op.UUID}, nil
}

func buildBatchTrash(op BatchOp) (thingscloud.Identifiable, map[string]string, error) {
	if op.UUID == "" {
		return nil, nil, fmt.Errorf("trash requires uuid")
	}

	u := newTaskUpdate().Trash(true)
	env := writeEnvelope{id: op.UUID, action: 1, kind: "Task6", payload: u.build()}

	return env, map[string]string{"cmd": "trash", "uuid": op.UUID}, nil
}

func buildBatchPurge(op BatchOp) (thingscloud.Identifiable, map[string]string, error) {
	if op.UUID == "" {
		return nil, nil, fmt.Errorf("purge requires uuid")
	}

	tombstoneUUID := generateUUID()
	payload := map[string]any{
		"dloid": op.UUID,
		"dld":   nowTs(),
	}
	env := writeEnvelope{id: tombstoneUUID, action: 0, kind: "Tombstone2", payload: payload}

	return env, map[string]string{"cmd": "purge", "uuid": op.UUID, "tombstone": tombstoneUUID}, nil
}

func buildBatchMoveToToday(op BatchOp) (thingscloud.Identifiable, map[string]string, error) {
	if op.UUID == "" {
		return nil, nil, fmt.Errorf("move-to-today requires uuid")
	}

	u := newTaskUpdate().Today()
	env := writeEnvelope{id: op.UUID, action: 1, kind: "Task6", payload: u.build()}

	return env, map[string]string{"cmd": "move-to-today", "uuid": op.UUID}, nil
}

func buildBatchMoveToProject(op BatchOp) (thingscloud.Identifiable, map[string]string, error) {
	if op.UUID == "" {
		return nil, nil, fmt.Errorf("move-to-project requires uuid")
	}
	if op.Project == "" {
		return nil, nil, fmt.Errorf("move-to-project requires project")
	}

	u := newTaskUpdate().Project(op.Project).Anytime()
	env := writeEnvelope{id: op.UUID, action: 1, kind: "Task6", payload: u.build()}

	return env, map[string]string{"cmd": "move-to-project", "uuid": op.UUID, "project": op.Project}, nil
}

func buildBatchMoveToArea(op BatchOp) (thingscloud.Identifiable, map[string]string, error) {
	if op.UUID == "" {
		return nil, nil, fmt.Errorf("move-to-area requires uuid")
	}
	if op.Area == "" {
		return nil, nil, fmt.Errorf("move-to-area requires area")
	}

	u := newTaskUpdate().Area(op.Area).Anytime()
	env := writeEnvelope{id: op.UUID, action: 1, kind: "Task6", payload: u.build()}

	return env, map[string]string{"cmd": "move-to-area", "uuid": op.UUID, "area": op.Area}, nil
}

func buildBatchEdit(op BatchOp) (thingscloud.Identifiable, map[string]string, error) {
	if op.UUID == "" {
		return nil, nil, fmt.Errorf("edit requires uuid")
	}

	u := newTaskUpdate()

	if op.Title != "" {
		u.Title(op.Title)
	}
	if op.Note != "" {
		u.Note(op.Note)
	}
	if op.When != "" {
		switch op.When {
		case "today":
			u.Today()
		case "anytime":
			u.Anytime()
		case "someday":
			u.Someday()
		case "inbox":
			u.Inbox()
		}
	}
	if op.Deadline != "" {
		if t := parseDate(op.Deadline); t != nil {
			u.Deadline(t.Unix())
		}
	}
	if op.Scheduled != "" {
		t, err := parseDateOrTime(op.Scheduled)
		if err != nil {
			return nil, nil, fmt.Errorf("scheduled: %w", err)
		}
		u.ScheduleDate(t.Unix())
	}
	if op.Project != "" {
		u.Project(op.Project)
		if op.When == "" {
			u.Anytime()
		}
	}
	if op.Area != "" {
		u.Area(op.Area)
		if op.When == "" {
			u.Anytime()
		}
	}
	if op.Heading != "" {
		u.Heading(op.Heading)
		if op.When == "" {
			u.Anytime()
		}
	}
	if len(op.Tags) > 0 {
		u.Tags(op.Tags)
	}
	if op.Repeat != "" {
		opts := map[string]string{"repeat": op.Repeat}
		if op.RepeatStart != "" {
			opts["repeat-start"] = op.RepeatStart
		}
		if op.Scheduled != "" {
			opts["scheduled"] = op.Scheduled
		}
		if op.When != "" {
			opts["when"] = op.When
		}
		plan, err := parseRepeatPlan(opts)
		if err != nil {
			return nil, nil, err
		}
		if plan != nil {
			if !plan.Clear && op.When == "inbox" {
				return nil, nil, fmt.Errorf("--repeat cannot be combined with inbox")
			}
			u.Repeater(plan.Rule)
			if !plan.Clear && op.When == "" && op.Scheduled == "" {
				u.ScheduleDate(plan.First.Unix())
			}
		}
	}

	env := writeEnvelope{id: op.UUID, action: 1, kind: "Task6", payload: u.build()}

	return env, map[string]string{"cmd": "edit", "uuid": op.UUID}, nil
}

// ---------------------------------------------------------------------------
// main() dispatch and usage
// ---------------------------------------------------------------------------

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: %s <command> [args]

Read commands (use an incremental local state cache):
  list [--today] [--inbox] [--anytime] [--someday] [--upcoming] [--search QUERY] [--area NAME] [--project NAME] [--simple|--format full|simple]
  today [--simple|--format full|simple]
  inbox [--simple|--format full|simple]
  anytime [--simple|--format full|simple]
  someday [--simple|--format full|simple]
  upcoming [--simple|--format full|simple]
  search <query> [--simple|--format full|simple]
  completed [--since RFC3339|YYYY-MM-DD] [--until RFC3339|YYYY-MM-DD] [--limit N] [--simple|--format full|simple]
  logbook [--since RFC3339|YYYY-MM-DD] [--until RFC3339|YYYY-MM-DD] [--limit N] [--simple|--format full|simple]
  show <uuid> [--simple|--format full|simple]
  areas
  projects
  tags

Normal list views hide completed tasks. Use completed/logbook for completion evidence.

Environment:
  THINGS_CLI_CACHE=/path/to/things-cli-state.json  Override read-state cache file

Write commands (fast - skip state loading):
  create "Title" [--note ...] [--when today|anytime|someday|inbox]
         [--deadline YYYY-MM-DD] [--scheduled YYYY-MM-DD]
         [--project UUID] [--heading UUID] [--area UUID]
         [--tags UUID,...] [--type task|project|heading] [--uuid UUID]
         [--checklist "Item 1,Item 2,..."] [--repeat SPEC] [--repeat-start YYYY-MM-DD] [--dry-run]
  create-area "Name" [--tags UUID,...] [--uuid UUID] [--dry-run]
  create-tag "Name" [--shorthand KEY] [--parent UUID] [--dry-run]
  add-checklist <task-uuid> "Item 1,Item 2,Item 3" [--dry-run]
  edit <uuid> [--title ...] [--note ...] [--when ...] [--deadline ...]
         [--scheduled ...] [--area UUID] [--project UUID]
         [--heading UUID] [--tags UUID,...] [--repeat SPEC|none] [--repeat-start YYYY-MM-DD] [--dry-run]
  complete <uuid> [--dry-run]
  trash <uuid> [--dry-run]
  purge <uuid> [--dry-run]
  move-to-today <uuid> [--dry-run]

Batch command (reads JSON from stdin, sends all ops in one HTTP request):
  batch [--dry-run]

  Example: echo '[{"cmd":"complete","uuid":"abc"},{"cmd":"trash","uuid":"def"}]' | %s batch

  Supported operations:
    {"cmd": "create", "title": "...", "note": "...", "when": "today|anytime|someday|inbox",
     "project": "uuid", "area": "uuid", "heading": "uuid", "tags": ["uuid",...],
     "repeat": "every-day|weekly:mon,wed|after-completion:every-day"}
    {"cmd": "complete", "uuid": "..."}
    {"cmd": "trash", "uuid": "..."}
    {"cmd": "purge", "uuid": "..."}
    {"cmd": "move-to-today", "uuid": "..."}
    {"cmd": "move-to-project", "uuid": "...", "project": "..."}
    {"cmd": "move-to-area", "uuid": "...", "area": "..."}
    {"cmd": "edit", "uuid": "...", "title": "...", "note": "...", ...}

  Repeat SPEC supports: every-day, daily, weekly, every-week, weekly:mon,wed,
    after-completion:every-day, after-completion:weekly:mon, none/off/clear.
    Unsupported: monthly/yearly/custom end conditions through CLI.
`, commandName(), commandName())
}

func Main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	if isHelpInvocation(os.Args[1:]) {
		printUsage()
		return
	}

	cmd := os.Args[1]
	args := os.Args[2:]
	dryRun := argsHaveBoolFlag(args, "dry-run")
	var ctx *cliContext
	if dryRun && commandNeedsHistoryHead(cmd) {
		ctx = &cliContext{}
	} else {
		ctx = initCLI(commandNeedsHistoryHead(cmd) && !dryRun)
	}

	switch cmd {
	// Read commands - need state
	case "list":
		cmdList(ctx.loadState(), args)
	case "today":
		cmdListWithOpts(ctx.loadState(), map[string]string{"today": "true"}, args)
	case "inbox":
		cmdListWithOpts(ctx.loadState(), map[string]string{"inbox": "true"}, args)
	case "anytime":
		cmdListWithOpts(ctx.loadState(), map[string]string{"anytime": "true"}, args)
	case "someday":
		cmdListWithOpts(ctx.loadState(), map[string]string{"someday": "true"}, args)
	case "upcoming":
		cmdListWithOpts(ctx.loadState(), map[string]string{"upcoming": "true"}, args)
	case "search":
		cmdSearch(ctx.loadState(), args)
	case "completed", "logbook":
		cmdCompleted(ctx.loadState(), args)
	case "show":
		requireArgs(args, 1, "things-cli show <uuid>")
		cmdShow(ctx.loadState(), args[0], args[1:])
	case "areas":
		cmdAreas(ctx.loadState())
	case "projects":
		cmdProjects(ctx.loadState())
	case "tags":
		cmdTags(ctx.loadState())

	// Write commands - skip state loading
	case "create":
		cmdCreate(ctx.history, args)
	case "create-area":
		cmdCreateArea(ctx.history, args)
	case "create-tag":
		cmdCreateTag(ctx.history, args)
	case "add-checklist":
		requireArgs(args, 2, `things-cli add-checklist <task-uuid> "Item 1,Item 2,Item 3"`)
		cmdAddChecklist(ctx.history, args[0], args[1:])
	case "edit":
		requireArgs(args, 1, "things-cli edit <uuid> [--title ...] [--note ...]")
		cmdEdit(ctx.history, args[0], args[1:])
	case "complete":
		args, dryRun = stripBoolFlag(args, "dry-run")
		requireArgs(args, 1, "things-cli complete <uuid>")
		cmdComplete(ctx.history, args[0], dryRun)
	case "trash":
		args, dryRun = stripBoolFlag(args, "dry-run")
		requireArgs(args, 1, "things-cli trash <uuid>")
		cmdTrash(ctx.history, args[0], dryRun)
	case "purge":
		args, dryRun = stripBoolFlag(args, "dry-run")
		requireArgs(args, 1, "things-cli purge <uuid>")
		cmdPurge(ctx.history, args[0], dryRun)
	case "move-to-today":
		args, dryRun = stripBoolFlag(args, "dry-run")
		requireArgs(args, 1, "things-cli move-to-today <uuid>")
		cmdMoveToToday(ctx.history, args[0], dryRun)
	case "batch":
		_, dryRun = stripBoolFlag(args, "dry-run")
		cmdBatch(ctx.history, dryRun)

	default:
		fatalf("unknown command: %s", cmd)
	}
}
