package events

import (
	"context"
	"time"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/ingress"
	"github.com/gopact-ai/steve/internal/ledger"
)

// FlowSnapshot is a bounded observation, not a consumer transport. Immutable
// ingress receipts survive event compression; claim fingerprints never expose
// the credential which authorizes an acknowledgment.
type FlowSnapshot struct {
	ObservedAt time.Time                     `json:"observed_at"`
	Capacity   int                           `json:"capacity"`
	Events     []FlowEvent                   `json:"events"`
	Claims     []FlowClaim                   `json:"claims"`
	Queues     map[string]durable.QueueStats `json:"queues"`
}
type FlowEvent struct {
	ID string `json:"id"`
	At string `json:"at"`
}
type FlowClaim struct {
	ID    string `json:"id"`
	Claim string `json:"claim"`
	Until string `json:"until"`
	At    string `json:"at"`
}

const flowEventTime = `CASE WHEN json_valid(data) THEN json_extract(data,'$.accepted_at') END`
const flowClaimTime = `CASE WHEN json_valid(data) THEN json_extract(data,'$.claim_until') END`
const flowInputKind = `CASE WHEN json_valid(data) THEN json_extract(data,'$.kind') END`
const flowHasClaim = `CASE WHEN json_valid(data) THEN coalesce(json_extract(data,'$.claim_token'),'')<>'' ELSE 0 END`

func init() {
	ledger.MustRegisterReadIndex("event_flow_input_v1", `CREATE INDEX IF NOT EXISTS event_flow_input_v1 ON bindings(kind,`+flowInputKind+`,`+flowEventTime+`,id)`)
	ledger.MustRegisterReadIndex("event_flow_claim_v1", `CREATE INDEX IF NOT EXISTS event_flow_claim_v1 ON bindings(kind,`+flowHasClaim+`,`+flowClaimTime+`,id)`)
}

func (s *Service) Flow(ctx context.Context) (out FlowSnapshot, err error) {
	out.Events = []FlowEvent{}
	out.Claims = []FlowClaim{}
	out.Queues = map[string]durable.QueueStats{}
	// The same global unfinished-delivery limit is enforced during routing.
	// A renderer's segment count is never a queue capacity.
	out.Capacity = s.Limits.Effective().Total
	err = s.Book.Read(ctx, func(tx *ledger.ReadTx) error {
		out.ObservedAt = time.Now().UTC()
		routing, err := durable.QueueStatsTx(tx, OutboxKind, EventKind, "$.at")
		if err != nil {
			return err
		}
		out.Queues["routing"] = routing
		for _, state := range []string{StatePending, StateClaimed, StateAccepted, StateUnknown} {
			var count durable.QueueStats
			if err := tx.QueryRow(`SELECT count(*),coalesce(min(json_extract(data,'$.updated_at')),'') FROM bindings WHERE kind=? AND (`+durable.StateExpression+`)=?`, DeliveryKind, state).Scan(&count.Pending, &count.Oldest); err != nil {
				return err
			}
			out.Queues[state] = count
		}
		rows, err := tx.Query(`SELECT json_extract(data,'$.adapter_receipt'),`+flowEventTime+` FROM bindings WHERE kind=? AND `+flowInputKind+`='event' AND `+flowEventTime+` IS NOT NULL ORDER BY `+flowEventTime+` DESC,id DESC LIMIT 64`, ingress.RecordKind)
		if err != nil {
			return err
		}
		for rows.Next() {
			var event FlowEvent
			if err := rows.Scan(&event.ID, &event.At); err != nil {
				rows.Close()
				return err
			}
			out.Events = append(out.Events, event)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		rows, err = tx.Query(`SELECT id,json_extract(data,'$.claim_token'),`+flowClaimTime+`,coalesce(json_extract(data,'$.claimed_at'),'') FROM bindings WHERE kind=? AND `+flowHasClaim+`=1 AND `+flowClaimTime+` IS NOT NULL ORDER BY `+flowClaimTime+` DESC,id DESC LIMIT 64`, DeliveryKind)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var claim FlowClaim
			var credential string
			if err := rows.Scan(&claim.ID, &credential, &claim.Until, &claim.At); err != nil {
				return err
			}
			claim.Claim = durable.Key(claim.ID, credential)
			if claim.At == "" {
				// Legacy claims used the same one-minute lease. New claims retain
				// their actual timestamp independently of later acknowledgments.
				until, err := time.Parse(time.RFC3339Nano, claim.Until)
				if err != nil {
					return err
				}
				claim.At = until.Add(-time.Minute).Format(time.RFC3339Nano)
			}
			out.Claims = append(out.Claims, claim)
		}
		return rows.Err()
	})
	return
}
