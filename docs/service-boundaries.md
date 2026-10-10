# Service boundary migration

Steve remains a modular Go server shared by its clients. ACP invokes an existing,
user-managed harness; service boundary work does not introduce a new harness or
microservices.

## Schedule handoff

`internal/schedule.ScheduleReceiver` receives a frozen firing and returns a
channel-defined receipt. `ReceiverRegistry` binds names explicitly; the application
composition root registers Console and Gateway adapters. Additional channels
register receivers without editing the dispatcher. Adapters delegate to the
existing receivers and preserve their authorization and admission checks.

The handoff does not collapse these distinct stages:

- Console returns an Exchange ID after durable admission; execution can continue.
- Gateway handles the scheduled input before returning its announcement Message ID.
- External delivery can remain unknown even if execution has completed.
- The schedule ledger records acceptance separately from the receiver's work.

The dispatcher retains BeginFiring, AcceptFiring, FailFiring, task rotation and
empty-receipt handling. The store retains the existing conservative recovery
policy: Console retries the same durable firing key and finds the original
exchange; every other channel's interrupted dispatch becomes unknown. Registering
a receiver grants no replay capability. A future explicit capability must be
supported by evidence of durable deduplication before broadening that policy.

## Next boundaries

1. Split turn source, actor and reply destination from execution admission while
   keeping existing entry points compatible. Preserve requester, ExpectedProject,
   ExpectedTask, ResumeAdmission and ExchangeID checks.
2. Introduce a neutral identity service using existing identity and configuration
   authority, removing Console's dependency on `feishu.owner_open_id`.
3. Evaluate a shared durable work entrance using the existing ledger and stable
   IDs, without prematurely merging Console's queue with Gateway's dispatch,
   reply and suppression responsibilities.
4. Add durable business events, subscriptions, provider consumption results and
   a fallback Inbox. SSE remains a frontend read-model feed.

Across these steps, retain command key + actor + payload identity, recovery of
original attempts for uncertain dispatch, conversation serialization, cancel and
steer, maintenance and retirement fencing, leases and stop proofs. Coordinator
responsibilities can then be narrowed across Conversation, Work, Execution,
Interaction and Automation. The current schedule change does not alter queues,
identity storage, external delivery recovery or task transport routing.
