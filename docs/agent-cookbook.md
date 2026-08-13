# Agent Cookbook

This cookbook gives agents and agent developers safe, copy-pasteable patterns
for working with Things Cloud through this repository.

Use `things-cloud-cli` for new integrations. `things-cli` remains a
backward-compatible alias.

## 1. Configure Credentials

Environment variables:

```bash
export THINGS_USERNAME='you@example.com'
export THINGS_TOKEN='your-things-cloud-password-or-token-alias'
```

Config file:

```json
{
  "username": "you@example.com",
  "token": "your-things-cloud-password-or-token-alias",
  "cache": "/path/to/things-cli-state.json"
}
```

Set `THINGS_CONFIG=/path/to/things-cloud.json` to use a custom config path.

## 2. Smoke Test Without Credentials

Help must work without credentials:

```bash
things-cloud-cli --help
```

Expected result: usage text and exit code `0`.

## 3. List Tasks for an Agent

Use compact JSON unless the caller asked for detailed metadata.

```bash
things-cloud-cli today --simple
things-cloud-cli inbox --simple
things-cloud-cli anytime --simple
things-cloud-cli someday --simple
things-cloud-cli upcoming --simple
```

Expected compact shape:

```json
[
  {
    "uuid": "task-uuid",
    "title": "Task title",
    "status": "open"
  }
]
```

## 4. Search Tasks

```bash
things-cloud-cli search "invoice" --simple
```

Use search when the user gives natural language references such as "the invoice
task" or "the task about the PR".

## 5. Create a Task Safely

Always preview agent-generated writes first:

```bash
things-cloud-cli create "Draft from agent" --when today --dry-run
```

If the user or host confirms the write:

```bash
things-cloud-cli create "Draft from agent" --when today
```

Schedule buckets:

- `inbox`
- `today`
- `anytime`
- `someday`

Use `--deadline YYYY-MM-DD` for deadline dates. Use `--scheduled YYYY-MM-DD` for
scheduled dates.

## 6. Create a Recurring Task Safely

Preview first:

```bash
things-cloud-cli create "Check car listings" --repeat every-day --dry-run
```

Execute after confirmation:

```bash
things-cloud-cli create "Check car listings" --repeat every-day
```

Supported repeat specs:

- `every-day`
- `daily`
- `weekly`
- `every-week`
- `weekly:mon,wed`
- `after-completion:every-day`
- `after-completion:weekly:mon`

Use `--repeat-start YYYY-MM-DD` to set the first reference date. Monthly,
yearly, repeat counts, and custom end dates are not exposed through the CLI yet.

## 7. Read Completion Evidence

Do not infer completion from a task disappearing from Today. Normal list views
hide completed tasks.

Use `completed` or `logbook`:

```bash
things-cloud-cli completed --since 2026-05-20T00:00:00Z --format full
things-cloud-cli logbook --since 2026-05-20 --limit 50 --format full
```

The full output includes UUID, title, `completedAt`, area/project/parent
metadata, and tag IDs for reconciliation.

## 8. Complete, Trash, or Move a Task

Dry-run first:

```bash
things-cloud-cli complete <task-uuid> --dry-run
things-cloud-cli trash <task-uuid> --dry-run
things-cloud-cli move-to-today <task-uuid> --dry-run
```

Then execute after confirmation:

```bash
things-cloud-cli complete <task-uuid>
things-cloud-cli trash <task-uuid>
things-cloud-cli move-to-today <task-uuid>
```

## 9. Edit a Task

Preview:

```bash
things-cloud-cli edit <task-uuid> --title "New title" --when anytime --dry-run
```

Execute:

```bash
things-cloud-cli edit <task-uuid> --title "New title" --when anytime
```

Clear repeat metadata:

```bash
things-cloud-cli edit <task-uuid> --repeat none --dry-run
things-cloud-cli edit <task-uuid> --repeat none
```

## 10. Batch Multiple Writes

Batch when an agent needs to apply several confirmed changes at once:

```bash
echo '[
  {"cmd": "create", "title": "Task 1", "when": "today", "repeat": "every-day"},
  {"cmd": "create", "title": "Task 2", "when": "anytime"},
  {"cmd": "complete", "uuid": "task-uuid"}
]' | things-cloud-cli batch --dry-run
```

Supported batch commands:

- `create`
- `complete`
- `trash`
- `purge`
- `move-to-today`
- `move-to-project`
- `move-to-area`
- `edit`

## 11. MCP Integration

Install:

```bash
go install github.com/pdurlej/things-cloud-sdk/cmd/things-mcp@latest
```

Example MCP config:

```json
{
  "mcpServers": {
    "things": {
      "command": "things-mcp",
      "env": {
        "THINGS_USERNAME": "you@example.com",
        "THINGS_TOKEN": "your-things-cloud-password-or-token-alias"
      }
    }
  }
}
```

Recommended MCP write policy:

- Call write tools with `dry_run: true` first.
- Show the planned change to the user.
- Call again with `dry_run: false` only after confirmation.

Core MCP tools:

- `list_tasks`
- `search_tasks`
- `create_task`
- `complete_task`
- `edit_task`
- `batch_tasks` (up to 50 create/edit/complete/trash operations in one write,
  same field semantics as the CLI `batch` command)
- `trash_task`
- `move_task_to_today`
- `add_checklist`
- `list_projects`
- `list_areas`
- `list_tags`

## 12. Long-Running Agent With Persistent Sync

Use `sync` when an agent or service needs to remember state across runs.

```go
client := things.New(things.APIEndpoint, username, password)
syncer, err := sync.Open("things.db", client)
if err != nil {
    return err
}
defer syncer.Close()

changes, err := syncer.Sync()
if err != nil {
    return err
}

for _, change := range changes {
    switch c := change.(type) {
    case sync.TaskCreated:
        fmt.Println("created", c.Task.Title)
    case sync.TaskCompleted:
        fmt.Println("completed", c.Task.Title)
    }
}
```

Use `QuickSync()` after the first successful sync when minimizing Cloud
round trips matters:

```go
changes, err := syncer.QuickSync()
```

## 13. Local SQLite Read-Only Mode

Use this only to inspect local Things data on macOS. Do not write to the local
database.

```go
reader, err := local.OpenDefault()
if err != nil {
    return err
}
defer reader.Close()

tasks, err := reader.Tasks(context.Background(), local.Query{
    Search: "invoice",
    Limit:  20,
})
```

## 14. What Agents Should Not Do

- Do not write to local Things SQLite.
- Do not create raw Things wire payloads unless the task explicitly requires it.
- Do not treat `st` as completion status. It is schedule.
- Do not treat `ss` as schedule. It is completion status.
- Do not skip `--dry-run` for destructive or user-visible writes.
- Do not infer completion from absence in normal task lists. Use
  `completed`/`logbook`.
- Do not store credentials or local sync DBs in git.
