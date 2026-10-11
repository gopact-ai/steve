# Event providers and client capabilities (v1)

Steve exposes three server modules: `events.v1`, `work.v1` and `clients.v1` in
`/state`'s `hub.services`. This protocol connects externally managed providers
and clients to the existing server. It does not install a mail service, replace
ACP, manage harness versions, or add a second model executor.

## Authority and enrollment

The owner uses **Connections & Events → Connections** or
`POST /console/connections` to create a connection:

```json
{
  "idempotency_key": "install-mail-2026-10",
  "name": "Mail provider",
  "kind": "provider",
  "event_types": ["mail.received"],
  "work": {"console:mail": "scratch"}
}
```

The response contains `grant` and a bearer `token`. Only a credential digest is
stored; the credential is returned once. Repeating enrollment with the same
key and declaration returns the existing grant with an empty token. A changed
declaration conflicts. After response loss, revoke the old connection and enroll
with a new key. Do not pass an owner token to a provider/client. Connections
cannot use `/console/*`; owner tokens cannot substitute for connection credentials
on `/services/*`. Revocation applies to new requests and is reread inside protected
transactions and Work admission.

Use `Authorization: Bearer <connection-token>` for every `/services/*` request.
Native project grants and existing channel actor IDs retain their old meanings.
Connection IDs are separate principals; matching a native actor's string confers
no authority. Work is explicitly delegated owner intelligence within a named
conversation/project pair, not a new external user's identity.

## Publish, subscribe and handle

`POST /services/events` accepts a stable producer key, declared event type and
JSON data (maximum 256 KiB):

```json
{
  "key": "mailbox/message-123",
  "type": "mail.received",
  "subject": "message-123",
  "payload": {"from": "sender@example.invalid", "subject": "Meeting"}
}
```

The authenticated source is fixed by the server. Reusing the key with changed
content or authority returns 409; the same immutable request returns its original
event. Publishing commits the event and its outbox together. Acceptance does not
prove that a consumer ran, a model completed, or an external reply was delivered.

Owners create subscriptions in the console or
`PUT /console/subscriptions/{id}`. Supply `revision: 0` to create; later writes
use the last observed revision. An identical retry of a lost save response returns
the saved revision. Event matching supports exact types or namespace patterns
such as `mail.*`, with an optional exact connection principal as `source`.

- `mode: "inbox"`: leave the event for the owner.
- `mode: "work"`: supply `conversation`, `project` and `instruction`. The
  conversation must already be bound to that project. Server automation submits
  the frozen rule through the same controlled Work port.
- `mode: "provider"`: supply an enrolled provider's grant ID as `handler`.

A delivery freezes the rule revision, instruction and destination. Disabling a
rule prevents pending claims/admission; it does not undo an already accepted
effect. Editing a rule applies to future deliveries. `include_derived` defaults
to false. Completion facts opt in explicitly and causal chains stop at eight
levels with an Inbox entry instead of starting more effects.

`POST /services/events/claim` returns `{found, delivery, event}`. When `found`
is true, deduplicate downstream effects by `delivery.id`; its `claim_token` is
evidence for that particular claim. A claim lasts one minute. A consumer returns:

```json
{"claim_token":"<original token>","state":"done","receipt":"<consumer receipt>"}
```

to `POST /services/events/deliveries/{id}/ack`. `done` requires a nonempty receipt;
`failed` and `unknown` may include a bounded `detail`. Acknowledgements are scoped
to the assigned provider. Repeating the exact terminal receipt is idempotent;
conflicting proof is rejected. After claim timeout or server restart, an uncertain
external effect becomes unknown. The original claim can still supply late proof.
Automatic retry is permitted only when the owner explicitly declares
`replay_safe` and the consumer durably deduplicates and retains receipts.

Unmatched external events, failed Work and unknown consumption appear in the
Inbox. Owners can inspect `/console/business-events/{id}`, resolve an Inbox item,
or explicitly reprocess an event after adding a subscription with
`POST /console/business-events/{id}/reprocess`. Reprocessing preserves deliveries
under existing revisions; a new revision creates a new delivery. Retrying a failed
or unknown delivery is an explicit operator action and replaces its claim token.
Do not retry an uncertain external effect without reconciling the original action.

## Call the user's intelligence

An authorized provider submits `POST /services/work`:

```json
{
  "key":"summarize/message-123",
  "conversation":"console:mail",
  "project":"scratch",
  "instruction":"Summarize the received message for the owner.",
  "context":{"message_id":"message-123","body":"External content"},
  "causation":"<delivery id, when processing a claimed delivery>"
}
```

Instructions are limited to 16 KiB and context to 256 KiB. Context is encoded as
untrusted data. Providers cannot submit platform controls such as `/clear`.
The server checks the existing project binding and scoped grant, and rereads the
grant and causal authority in the queue-admission transaction. A provider can
reference only its own claimed/done delivery. An independent provider job may
omit `causation`; it still requires an explicit Work grant.

`GET /services/work/{id}` returns `{work, result}` for that connection. Pending,
accepted and failed admission are distinct from execution's queued/running/done
result. The Work record and original Console Exchange ID commit with queue
admission. The final original reply is retained on the Work record. Stable-key
retries resume that identity; they do not reset failed work or start another ACP
turn to conceal uncertainty. A changed request conflicts.

## Advertise and use client capabilities

Enroll with `kind: "client"` and explicit capability grants:

```json
{
  "idempotency_key":"connect-mac-2026-10",
  "name":"Owner Mac",
  "kind":"client",
  "capabilities":[{
    "client":"",
    "capability":"notifications.show",
    "conversation":"console:mail",
    "approval":true
  }]
}
```

The server fills the client ID. The client registers with
`POST /services/client/register`, supplying `generation: 0` initially and its
capabilities (`name`, `description`, `input_schema` with `type: "object"`). Reconnect
using the last registration generation. An identical lost-response retry returns
the existing generation. Registration is limited to 32 granted capabilities and
does not expand authority. Heartbeat every 15 seconds with
`POST /services/client/heartbeat` and the current generation; availability expires
after 45 seconds.

Capability invocations are created through `/services/client/invoke` with `key`,
`client`, `capability`, `conversation`, and object `arguments`; external callers
need their own explicit capability delegation as well as the client's permission.
Private Console ACP executions use `steve_client_capabilities`,
`steve_client_invoke` and `steve_client_result`. The bridge fixes the original
task/attempt/epoch/process generation itself. Shared-channel and delegated
sessions cannot use these private capabilities. Dispatch rereads the original
execution and live lease, so an ended attempt cannot cause a later native effect.

When required, an owner approval is shown in Inbox before dispatch. The client
polls `POST /services/client/claim` with its generation and receives
`{found, invocation}`. Validate arguments against the advertised schema locally;
the server bounds the envelope and enforces its object shape, scopes and fences,
but does not execute a general JSON Schema validator or native capability code.

Complete with `POST /services/client/invocations/{id}/complete`:

```json
{"claim_token":"<original token>","state":"done","receipt":"<client receipt>","result":{"shown":true}}
```

Use the invocation ID for local effect deduplication and persist the result before
acknowledging. Claims last one minute. A missing receipt becomes unknown and is
never automatically reissued. The original token can settle that invocation after
restart; registering a new generation is not evidence that an old effect stopped.
Client approvals and unknown results have their own actionable Inbox projection;
they are also durable business facts, without duplicate fallback rows by default.

## Operational boundaries

All service records use the existing ledger transaction and replication path.
There is no second message store. Management lists use 100-item keyset pages with
`next` cursors. Worker passes rotate bounded pages so early unresolved work cannot
starve later records. Claims require live credentials, and native clients are
responsible for their own local effect validation, deduplication and evidence.

These connection grants and in-flight records are hub-owned service state, not
project packages. Exporting a project does not copy connection credentials or
implicitly authorize providers on another hub. Create destination connections
explicitly and reconcile any original unfinished work before removal/migration.
Existing Skills/MCP runtime bundles remain fixed per harness session. No native
provider, iOS application, A2A adapter, automatic deployment or model version
management is installed by these APIs.
