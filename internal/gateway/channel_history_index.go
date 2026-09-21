package gateway

import (
	"database/sql/driver"
	"encoding/json"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"modernc.org/sqlite"
)

// These are local, rebuildable facts, not an execution queue or a second history.
// Go decoding deliberately matches the receipt owners (duplicate keys, case
// folding, nulls and type errors); SQLite json_extract does not have that contract.
const channelInputPredicate = `kind IN ('gateway-input','gateway-recovery-input') AND finished_at IS NOT NULL AND error=''`
const channelTopicPredicate = `kind='gateway-input-topic' AND finished_at IS NOT NULL AND error=''`
const channelDispatchPredicate = `kind IN ('gateway-input-dispatch','gateway-recovery-dispatch') AND finished_at IS NOT NULL AND error=''`
const channelProofPredicate = `kind IN ('gateway-input-reply','gateway-recovery-reply','gateway-input-suppressed') AND finished_at IS NOT NULL AND error=''`

func channelFact(alias, field string) string {
	if alias != "" {
		alias += "."
	}
	return `steve_channel_fact_v1(` + alias + `kind,` + alias + `actor,` + alias + `result,'` + field + `')`
}
func channelPredicate(alias, predicate string) string {
	for _, name := range []string{"kind", "finished_at", "error"} {
		predicate = strings.ReplaceAll(predicate, name, alias+"."+name)
	}
	return predicate
}
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("steve_channel_fact_v1", 4, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		kind, _ := args[0].(string)
		actor, _ := args[1].(string)
		field, _ := args[3].(string)
		var raw []byte
		switch v := args[2].(type) {
		case string:
			raw = []byte(v)
		case []byte:
			raw = v
		default:
			return nil, nil
		}
		return channelRecordFact(kind, actor, raw, field), nil
	})
	inputFields := channelFact("", "conversation") + `,rtrim(received_at,'Z'),id,kind,actor,` + channelFact("", "topic") + `,` + channelFact("", "title")
	ledger.MustRegisterReadIndex("commands_channel_input", `CREATE INDEX IF NOT EXISTS commands_channel_input ON commands(`+inputFields+`) WHERE `+channelInputPredicate)
	ledger.MustRegisterReadIndex("commands_channel_input_id", `CREATE INDEX IF NOT EXISTS commands_channel_input_id ON commands(id,`+inputFields+`) WHERE `+channelInputPredicate)
	topicFields := channelFact("", "thread") + `,id,actor,` + channelFact("", "input")
	ledger.MustRegisterReadIndex("commands_channel_topic", `CREATE INDEX IF NOT EXISTS commands_channel_topic ON commands(`+topicFields+`) WHERE `+channelTopicPredicate)
	ledger.MustRegisterReadIndex("commands_channel_topic_id", `CREATE INDEX IF NOT EXISTS commands_channel_topic_id ON commands(id,`+topicFields+`) WHERE `+channelTopicPredicate)
	ledger.MustRegisterReadIndex("commands_channel_dispatch", `CREATE INDEX IF NOT EXISTS commands_channel_dispatch ON commands(id,kind,actor,`+channelFact("", "output")+`,rtrim(finished_at,'Z'),`+channelFact("", "project")+`) WHERE `+channelDispatchPredicate)
	ledger.MustRegisterReadIndex("commands_channel_proof", `CREATE INDEX IF NOT EXISTS commands_channel_proof ON commands(id,kind,actor,`+channelFact("", "input")+`,rtrim(finished_at,'Z')) WHERE `+channelProofPredicate)
}

func channelRecordFact(kind, actor string, raw []byte, field string) driver.Value {
	switch kind {
	case gatewayInputKind:
		now := time.Time{}
		input, err := decodeGatewayInput(ledger.CommandRecord{Kind: kind, Actor: actor, Result: raw, FinishedAt: &now})
		if err != nil {
			return nil
		}
		switch field {
		case "conversation":
			return conversationID(input.Message)
		case "topic":
			text, topic := topicTask(input.Message)
			if topic && text != "" && conversationID(input.Message) == input.Message.ChatID {
				return int64(1)
			}
			return int64(0)
		case "title":
			return channelTitle(input.Message.Text)
		}
	case recoveryInputKind:
		var input recoveryInput
		if json.Unmarshal(raw, &input) != nil || input.Requester != actor || input.ConversationID == "" || input.MessageID == "" {
			return nil
		}
		switch field {
		case "conversation":
			return input.ConversationID
		case "topic":
			return int64(0)
		case "title":
			return channelTitle(input.Prompt)
		}
	case "gateway-input-topic":
		var proof topicReceipt
		if json.Unmarshal(raw, &proof) != nil || proof.InputID == "" || proof.Anchor == "" || proof.Thread == "" {
			return nil
		}
		switch field {
		case "thread":
			return proof.Thread
		case "input":
			return proof.InputID
		}
	case "gateway-input-dispatch", "gateway-recovery-dispatch":
		var output recoveredOutput
		if strings.TrimSpace(string(raw)) != "null" && json.Unmarshal(raw, &output) == nil {
			if field == "output" {
				return int64(1)
			}
			if field == "project" && output.Result.Injected != nil {
				return output.Result.Injected.Project
			}
		}
	case "gateway-input-reply", "gateway-recovery-reply", "gateway-input-suppressed":
		var proof ledger.CommandProof
		if field == "input" && json.Unmarshal(raw, &proof) == nil && proof.CommandID != "" && proof.Receipt != "" {
			return proof.CommandID
		}
	}
	return nil
}

func channelTitle(input string) string {
	input = strings.TrimSpace(input)
	if strings.HasPrefix(input, "/") {
		return ""
	}
	line, _, _ := strings.Cut(input, "\n")
	runes := []rune(strings.TrimSpace(line))
	if len(runes) > 48 {
		runes = runes[:48]
	}
	return string(runes)
}

// Both arms start from an expression-index seek. The second arm can only
// redirect a /topic input through its own successful route, never /attempt.
// Route checks mirror topicMessage without decoding every historical input.
func channelTurnsSQL(boundary string) string {
	input := channelPredicate("i", channelInputPredicate)
	topic := channelPredicate("t", channelTopicPredicate)
	validRoute := `t.id=i.id||'/topic' AND t.actor=i.actor AND ` + channelFact("t", "input") + `=i.id AND ` + channelFact("i", "topic") + `=1 AND ` + channelFact("t", "thread") + `<>` + channelFact("i", "conversation")
	fields := `i.id,i.kind,i.actor,rtrim(i.received_at,'Z') AS at,` + channelFact("i", "title") + ` AS title`
	return `SELECT ` + fields + `,CASE WHEN ` + channelFact("i", "topic") + `=1 OR (i.kind='gateway-input' AND EXISTS (SELECT 1 FROM commands WHERE id=i.id||'/topic')) THEN 0 ELSE 1 END AS output_safe FROM commands AS i INDEXED BY commands_channel_input
 WHERE ` + input + ` AND ` + channelFact("i", "conversation") + `=?` + boundary + `
 AND NOT EXISTS (SELECT 1 FROM commands AS t INDEXED BY commands_channel_topic_id WHERE ` + topic + ` AND ` + validRoute + `)
 UNION ALL SELECT ` + fields + `,1 AS output_safe FROM commands AS t INDEXED BY commands_channel_topic
 JOIN commands AS i INDEXED BY commands_channel_input_id ON i.id=` + channelFact("t", "input") + `
 WHERE ` + topic + ` AND ` + input + ` AND ` + channelFact("t", "thread") + `=? AND ` + validRoute + boundary
}

func channelCandidatesSQL() string {
	return `SELECT conversation FROM (SELECT DISTINCT ` + channelFact("", "conversation") + ` AS conversation FROM commands INDEXED BY commands_channel_input
 WHERE ` + channelInputPredicate + ` AND ` + channelFact("", "conversation") + `>? ORDER BY conversation LIMIT ?)
 UNION SELECT conversation FROM (SELECT DISTINCT ` + channelFact("", "thread") + ` AS conversation FROM commands INDEXED BY commands_channel_topic
 WHERE ` + channelTopicPredicate + ` AND ` + channelFact("", "thread") + `>? ORDER BY conversation LIMIT ?) ORDER BY conversation LIMIT ?`
}

func channelSummarySQL() string {
	// Correlated point reads avoid LEFT JOIN's null-row expression re-evaluation;
	// every selected fact comes from an index, not decoded dispatch bodies.
	dispatchWhere := channelPredicate("d", channelDispatchPredicate) + `
 AND x.output_safe=1 AND d.id=x.id||'/dispatch' AND d.kind=CASE x.kind WHEN 'gateway-input' THEN 'gateway-input-dispatch' ELSE 'gateway-recovery-dispatch' END AND d.actor=x.actor AND ` + channelFact("d", "output") + `=1`
	dispatch := func(field string) string {
		return `(SELECT ` + field + ` FROM commands AS d INDEXED BY commands_channel_dispatch WHERE ` + dispatchWhere + `)`
	}
	proof := func(suffix, kind string) string {
		return `(SELECT rtrim(p.finished_at,'Z') FROM commands AS p INDEXED BY commands_channel_proof WHERE ` + channelPredicate("p", channelProofPredicate) + ` AND x.output_safe=1 AND p.id=x.id||'` + suffix + `' AND p.kind=` + kind + ` AND p.actor=x.actor AND ` + channelFact("p", "input") + `=x.id)`
	}
	return `SELECT * FROM (WITH turns AS (` + channelTurnsSQL("") + `), facts AS MATERIALIZED (
 SELECT x.*,` + dispatch(`rtrim(d.finished_at,'Z')`) + ` AS dispatch_at,` + dispatch(channelFact("d", "project")) + ` AS project,` +
		proof("/reply", `CASE x.kind WHEN 'gateway-input' THEN 'gateway-input-reply' ELSE 'gateway-recovery-reply' END`) + ` AS reply_at,` +
		proof("/suppressed", `CASE x.kind WHEN 'gateway-input' THEN 'gateway-input-suppressed' ELSE '' END`) + ` AS suppressed_at FROM turns AS x)
 SELECT COALESCE(SUM(1+CASE WHEN dispatch_at IS NOT NULL OR reply_at IS NOT NULL OR suppressed_at IS NOT NULL THEN 1 ELSE 0 END),0),
 MAX(MAX(at,COALESCE(dispatch_at,at),COALESCE(reply_at,at),COALESCE(suppressed_at,at))),
 (SELECT title FROM facts WHERE title<>'' ORDER BY at,id LIMIT 1),
 (SELECT project FROM facts WHERE project<>'' ORDER BY at DESC,id DESC LIMIT 1)
 FROM facts)`
}
