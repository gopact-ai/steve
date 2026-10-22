# Client protocol `steve.client.v1`

`steve.client.v1` is the HTTP protocol a Steve client (a terminal client, the desktop app, the web console) uses to work with one Steve server. It is served under `/v1/` on the same listener and port as the console.

The message types are described by a JSON Schema, [`protocol/steve.client.v1.schema.json`](protocol/steve.client.v1.schema.json), generated from the server's own types.

## Compatibility

The protocol only grows. Within v1 a field, an enum value, a capability, an event type or a method may be added; none is renamed, retyped or removed. Clients ignore fields and enum values they do not know, and check `capabilities` before relying on an optional method. Optional fields may be absent, and an absent field means the same as its zero value.

## Authentication and language

Every method requires the console's owner token as `Authorization: Bearer <token>`. An `EventSource` that cannot set headers may pass `?token=<token>`. A missing or wrong token is answered with `401` and a plain-text body.

Requests may send `Accept-Language`. Server text (errors, receipts, replies to commands) is in that language, otherwise in the server's default. `GET /v1/initialize` reports the language chosen.

## Errors

Every refusal other than `401` has a JSON body:

```json
{"error": {"code": "command_conflict", "message": "command_id was already used with a different submission"}}
```

| Status | `code` | Meaning |
| --- | --- | --- |
| 400 | `bad_request` | The request is malformed or names an invalid value. Unknown body fields are refused. |
| 403 | `forbidden` | The thread belongs to a channel and is read-only, or the question belongs to someone else. |
| 404 | `not_found` | The thread, turn, question, task or line does not exist. |
| 409 | `command_conflict` | The `command_id` was already used with different content. |
| 409 | `project_conflict` | `expected_project` differs from the thread's project. Nothing was queued. |
| 409 | `conflict` | The object has moved on: the turn is no longer queued or has ended, or the question is resolved. |
| 409 | `stale_cursor` | A page cursor no longer matches the data. Read again from the start. |
| 501 | `not_implemented` | This server is not set up to serve the method. |
| 503 | `unavailable` | The server is shutting down, or a dependency cannot be read right now. Retry. |

## Objects

- A **thread** is one conversation. Threads with `transport: "console"` accept turns. Other threads are read-only histories of a chat channel (`read_only: true`).
- A **turn** is one input to a thread and what became of it. `state` is `queued`, `running`, `done`, `failed` or `cancelled`. A running turn may carry a `phase`: `recovering` while Steve resumes it after a restart, `awaiting-user` while it waits for an answer.
- An **item** is one entry in a thread's transcript. `kind` is one of:
  - `message` (`role` `user` or `assistant`)
  - `reasoning`
  - `tool_call` (a tool call or a delegated step)
  - `file_change` (a summary of the files an attempt changed)
  - `question`
  - `milestone`
  - `notice`

  The items that make up a reply come before the reply's `message` and share its `turn`. The reply's own ID is the turn's `reply_item`.
- A **question** is a decision an agent is waiting on: a `permission` request or a `question`.
- A **task** is one unit of tracked work, including delegated children.
- An **event** is one change. Its `cursor` is opaque and increases within one server run.

## Methods

| Method | Purpose |
| --- | --- |
| `GET /v1/initialize` | Protocol, server version, capabilities, language |
| `GET /v1/threads` | Every thread, running first, then most recent |
| `GET /v1/threads/{id}` | One thread, its live turns and a page of items |
| `POST /v1/threads` | Create a thread bound to a project and, optionally, an agent |
| `POST /v1/turns` | Submit input to a thread |
| `POST /v1/turns/{id}/steer` | Start a queued turn now, interrupting the running one |
| `POST /v1/turns/{id}/interrupt` | Remove a queued turn or stop a running one |
| `POST /v1/questions/{id}/answer` | Decide a pending question |
| `GET /v1/attempts/{id}/diff` | The files an attempt changed, and one file's diff |
| `GET /v1/projects` | Declared projects |
| `GET /v1/agents` | Registered agents and whether they can run |
| `GET /v1/tasks` | Task history, one page at a time |
| `GET /v1/events` | Server-sent change stream |

### `GET /v1/initialize`

```json
{"protocol":"steve.client.v1","server_version":"v1.4.0 (3c9d2e1)","capabilities":["projects","agents","tasks","events","threads","turns","steer","interrupt","questions","material_refs","interactive_requests","attempt_diff"],"locale":"en"}
```

| Capability | What it means |
| --- | --- |
| `threads`, `turns`, `steer`, `interrupt`, `questions` | The server can act in threads |
| `material_refs` | `refs` on a turn are kept with the turn |
| `interactive_requests` | Agents can ask questions during a turn |
| `channel_threads` | Channel histories are listed and readable |
| `attempt_diff` | Attempt changes can be read |
| `projects`, `agents`, `tasks`, `events` | The matching read methods |

### Threads

`GET /v1/threads/{id}?limit=&cursor=` returns `thread`, `turns` (the thread's queue: queued, running and recently ended turns) and `items`, oldest first. `limit` is 1 to 500 (default 100). The page holds the newest items. To read further back, pass the `next_cursor` you received as `cursor`. `next_cursor` is absent on the page that reaches the start of the thread. A question appears as an item at the moment it was asked, with its current state.

`POST /v1/threads` creates a thread:

```json
{"id":"console:3f2a","project":"scratch","agent":"codex"}
```

The client may choose `id` so that a retry names the same thread. An ID without the `console:` prefix gets it. Without an `id` the server generates one. Creating a thread with the same ID and project again changes nothing. Naming another project for an existing thread is refused. `agent` sets the thread's agent. The response is the thread as `GET /v1/threads/{id}` returns it.

### Turns

```json
{"thread":"console:3f2a","input":"Fix the failing test","command_id":"b9e1…","expected_project":"scratch"}
```

`command_id` is required and identifies the submission within its thread:

- Sending the same `command_id` with the same content again returns the original turn, even after the server restarts. Nothing runs twice.
- Sending it with different content is refused with `command_conflict`.
- The console uses the same identity, so a turn sent through either API is the same turn.

`expected_project` refuses the turn with `project_conflict` when the thread is bound to another project. Optional fields are:

- `quotes`: earlier items carried along, as `{"thread","item"}`.
- `refs`: stored materials. Available when `material_refs` is listed.
- `locale`: `zh` or `en`.
- `rewind_to`: replaces an earlier user message and everything after it.

The response is the turn. Input starting with `/` is a Steve command and runs as a turn like any other.

`POST /v1/turns/{id}/steer` starts a queued turn now and interrupts the one running. The turn must still be queued.

`POST /v1/turns/{id}/interrupt` takes `{"command_id":"…"}`.

- A queued turn is removed. `turn.state` is `cancelled`.
- A running turn is stopped by a `/cancel` control turn keyed by that `command_id`. It is returned as `control`, and retrying with the same `command_id` returns the same control.
- A turn that has ended is a `conflict`.

### Questions

`POST /v1/questions/{id}/answer`:

```json
{"command_id":"a1","decision":"accept","choice":"allow"}
```

`decision` is `accept`, `decline` or `cancel`. `choice` must be one of the question's `options`. `text` is accepted when `allow_free_text` is true. Repeating the same answer returns the same receipt. Any other decision on a resolved question is a `conflict`.

### Attempts, projects, agents, tasks

`GET /v1/attempts/{id}/diff` lists the changed `files` (status `A`, `M` or `D` with line counts). With `?path=` it also returns that file's unified `diff`. `truncated` marks output cut at the server's limit.

`GET /v1/tasks` accepts these parameters:

- `thread` or `project` (not both)
- `status`: `all`, `live` or `closed`
- `limit`: 1 to 100, default 50
- `cursor`: the `next_cursor` of the previous page

A cursor that no longer matches the data is a `stale_cursor`.

### Events

`GET /v1/events?thread=&after=` is a `text/event-stream`. Each event is sent as:

```
id: <cursor>
event: <type>
data: {"cursor":"…","type":"item.completed","thread":"console:3f2a","turn":"e…","item":"r…","source_kind":"console.reply", …}
```

| `type` | When |
| --- | --- |
| `thread.updated` | A thread was created or its title changed |
| `thread.deleted` | A thread was deleted |
| `turn.updated` | A thread's queue changed |
| `turn.progress` | A running turn or one of its delegated steps reported `progress` |
| `item.completed` | A transcript item was recorded; `silent` items are not shown |
| `item.removed` | An item left the transcript (rewound or recalled) |
| `question.updated` | A question was asked or changed state |
| `task.updated` | A task changed |
| `server.event` | Any other server change; `source_kind` names it |

Events say what changed, not the full object. A client re-reads the thread or list affected.

The server first replays recent events, then streams live ones, with a `: keepalive` comment every 20 seconds.

To resume, send the last `cursor` you received, as the `Last-Event-ID` header or as `after`. If both are sent, the later one is used. If the server can no longer continue from that cursor (it restarted, or the events after it are no longer kept), it first sends `event: reset` and then every recent event. On a reset, the client re-reads what it shows. A client that falls too far behind is disconnected and reconnects with its last cursor.

`thread` limits the stream to one thread's events. A reset is sent either way.
