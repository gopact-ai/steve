# Event providers and client capabilities

Steve exposes `events.v1`, `work.v1` and `clients.v1` through authenticated HTTP services. Availability is reported in `/state` under `hub.services`. Providers run outside Steve; Work uses the project's existing agent execution and permissions.

The [mail](../providers/mail/README.md) and [GitHub](../providers/github/README.md) providers implement this protocol. For console usage, see [event subscriptions](events.md).

## Connections

An owner creates a scoped connection with `POST /console/connections`:

```json
{
  "idempotency_key": "mail-connection-1",
  "name": "Mail provider",
  "kind": "provider",
  "event_types": ["mail.received"],
  "work": {"console:mail": "scratch"}
}
```

The response contains a `grant` and a bearer `token`, returned once. An identical retry returns the grant with an empty token; changed declarations conflict. Only credential digests are stored. The grant ID and principal remain stable across rotation and scope edits.

Use `Authorization: Bearer <connection-token>` on `/services/*`. Connection credentials cannot access `/console/*`, and owner tokens cannot replace connection credentials. A Work grant delegates access to the named conversation/project; it does not create a native user account.

Rotate a credential with `POST /console/connections/{id}/credential`:

```json
{"revision":1,"idempotency_key":"mail-rotation-1"}
```

Use the current `credential_revision`. The replacement token is returned once; the old generation loses authority. An identical retry does not rotate again. If the token response was lost, read the current revision and explicitly rotate with a new key.

Replace the complete scope with `PUT /console/connections/{id}/scope`:

```json
{
  "revision":1,
  "name":"Mail provider",
  "event_types":["mail.received"],
  "work":{"console:mail":"scratch"},
  "capabilities":[]
}
```

Use `scope_revision`, independently of credential revision. Omitted collections remove their grants. Identical retries are idempotent; stale changed writes return 409. Connection kind and identity stay fixed. Retirement revokes the connection; rotation cannot reactivate it. Owner audit records are available at `/console/connection-audit`.

## Provider registration

`POST /services/provider/register` declares metadata and event contracts:

```json
{
  "generation":0,
  "plugin_id":"mail.host",
  "version":"1.0.0",
  "events":[{
    "type":"mail.received",
    "version":"1",
    "name":"Email received",
    "description":"A new inbox message",
    "i18n":{"zh-CN":{"name":"收到邮件","description":"收到一封新邮件。"}},
    "schema":{"type":"object","properties":{"subject":{"type":"string"}}}
  }]
}
```

Registration cannot expand the connection grant. Each provider/type/version has an immutable canonical contract digest. Change the version when changing the schema or presentation metadata. Schemas describe the contract; publishers and consumers validate payloads themselves.

Reconnect using the last observed generation. Identical retries return that registration. Send `POST /services/provider/heartbeat` with `{"generation":1}` every 15 seconds; availability expires after 45 seconds. A stale generation cannot refresh another process's health. Credential rotation preserves registration generation.

`GET /services/provider` reads the caller's declaration. Owners list `/console/event-providers?cursor=...`. Providers without registration are labelled unregistered, not inferred offline.

| Registration limit | Value |
| --- | --- |
| Unique event types | 64 |
| Name / description | 256 / 2,048 bytes per language |
| Translations | 16 per contract |
| Presentation metadata | 16 KiB per contract; 256 KiB per registration |

Language tags are canonical BCP 47. Each display field resolves by console locale, base/regional alternatives, default text, English, then another translation. Event type remains the routing identity. Empty display metadata is allowed.

## Publishing events

`POST /services/events` accepts an immutable request with a stable producer key:

```json
{
  "key":"mailbox/message-123",
  "type":"mail.received",
  "subject":"message-123",
  "schema_version":"1",
  "payload":{"from":"sender@example.invalid","subject":"Meeting"}
}
```

JSON data is limited to 256 KiB. The server fixes the authenticated source and commits the event with its outbox. Reusing a key with identical content returns the original event; changed content or authority returns 409.

Registered providers can select `schema_version`; omission selects the current declaration. Acceptance freezes the contract, translations and digest. Later registration does not change historical events or identical retry results. Unregistered providers may omit a version. Withdrawn or incompatible explicit versions cannot create new events.

Acceptance confirms persistence, not consumer execution, model completion or external delivery.

## Subscriptions and delivery

Owners use `PUT /console/subscriptions/{id}` with `revision: 0` to create, and the last observed revision to edit. Exact types and namespace patterns such as `mail.*` are supported, with optional exact connection principal `source`.

| Mode | Required destination |
| --- | --- |
| `inbox` | Owner Inbox |
| `work` | `conversation`, `project`, `instruction`; the conversation is already bound to the project |
| `provider` | Enrolled grant ID in `handler` |

Each delivery freezes the rule revision and destination. Disabling prevents pending claims/admission, without undoing accepted effects. `include_derived` defaults to false; effect chains stop at eight causal levels and leave an Inbox entry.

Consumers call `POST /services/events/claim`, which returns `{found, delivery, event}`. Deduplicate effects by `delivery.id` and retain the original claim token. Claims last one minute. Acknowledge at `POST /services/events/deliveries/{id}/ack`:

```json
{"claim_token":"<original token>","state":"done","receipt":"<durable consumer receipt>"}
```

`done` requires a nonempty receipt. `failed` and `unknown` may include `detail`. Exact terminal acknowledgements are idempotent; conflicting evidence is rejected. A timed-out or interrupted uncertain effect becomes unknown; the original claim can still submit late proof. Automatic retry requires explicit `replay_safe` and durable consumer deduplication.

Owners inspect `/console/business-events/{id}` and `/console/event-deliveries/{id}`. Unmatched events, failed Work and unknown consumption appear in Inbox. `POST /console/business-events/{id}/reprocess` preserves existing-revision deliveries; a new revision creates a new delivery. Explicit retry replaces the claim token, so reconcile uncertain effects first.

## Work requests

An authorized connection calls `POST /services/work`:

```json
{
  "key":"summarize/message-123",
  "conversation":"console:mail",
  "project":"scratch",
  "instruction":"Summarize the message for the owner.",
  "context":{"message_id":"message-123","body":"External content"},
  "causation":"<delivery id>"
}
```

Instructions are limited to 16 KiB, context to 256 KiB. Context is untrusted data. Platform controls are not accepted. Admission rechecks the scoped grant, project binding and causal authority. A connection may reference only its own claimed/done delivery; independent jobs may omit causation but still require a Work grant.

`GET /services/work/{id}` returns `{work, result}` for the connection. Admission states and queued/running/done execution results are separate. Work retains the original input receipt and final reply. Stable-key retries return or resume that identity, never reset failure or conceal uncertainty by starting a new turn.

## Client capabilities

Enroll a client with explicit conversation-scoped capabilities:

```json
{
  "idempotency_key":"desktop-client-1",
  "name":"Owner desktop",
  "kind":"client",
  "capabilities":[{
    "client":"",
    "capability":"notifications.show",
    "conversation":"console:mail",
    "approval":true
  }]
}
```

The server fills the client ID. `POST /services/client/register` accepts initial `generation: 0` and up to 32 granted capabilities, each with `name`, `description` and object `input_schema`. Reconnect with the last generation; heartbeat through `/services/client/heartbeat` every 15 seconds. Availability expires after 45 seconds.

`POST /services/client/invoke` accepts `key`, `client`, `capability`, `conversation` and object `arguments`. External callers need their own delegation as well as the client's permission. Private owner ACP sessions use `steve_client_capabilities`, `steve_client_invoke` and `steve_client_result`; shared and delegated sessions cannot inherit these private capabilities.

Dispatch checks the original task, attempt, epoch, process generation and live lease. Standalone executions also bind native session and Hub activation. Ended executions and stale activations cannot dispatch later native effects.

Required approval appears in Inbox. The client polls `POST /services/client/claim` with its generation, receives `{found, invocation}`, validates arguments locally, and completes at `POST /services/client/invocations/{id}/complete`:

```json
{"claim_token":"<original token>","state":"done","receipt":"<durable client receipt>","result":{"shown":true}}
```

Persist deduplication by invocation ID before acknowledging. Claims last one minute. Missing proof becomes unknown and is not automatically reissued. The original token can settle after restart; a new client generation does not prove the old action stopped. The server checks object shape, bounds and authority, not general JSON Schema semantics or native execution.

## Observation and storage

Owner APIs provide read-only views:

| Endpoint | Contents |
| --- | --- |
| `/console/business-events?cursor=...` | Up to 32 event summaries per keyset page |
| `/console/event-activity` | Recent grouped activity and unresolved attention |
| `/console/event-flow` | Queue counts and bounded recent intake/claim samples |
| `/console/service-backlog` | Intake limits, pending age and storage |

Flow animation reflects sampled activity, not lossless transport or measured throughput. Inspection never claims or retries work. Count units differ: one event can have multiple deliveries and attention entries.

Management lists use keyset pagination; follow returned cursors. Recovery uses bounded rotating passes. Records share the existing ledger and replication path. Project migration does not copy connection credentials or authorize destination providers. Consumers remain responsible for local validation, effect receipts and reconciliation.
