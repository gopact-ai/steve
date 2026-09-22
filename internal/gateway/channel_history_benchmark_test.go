package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/turn"
)

// All fixtures use isolated SQLite files. Setup is outside the timer; measured
// operations include the complete public read, not just candidate selection.
func BenchmarkChannelHistory(b *testing.B) {
	for _, fixture := range []struct{ turns, conversations, bodyBytes int }{
		{1000, 1, 1024}, {1000, 1, 64 << 10}, {10000, 1, 1024}, {10000, 1, 64 << 10}, {10000, 100, 64 << 10},
	} {
		b.Run(fmt.Sprintf("turns=%d/conversations=%d/body=%d", fixture.turns, fixture.conversations, fixture.bodyBytes), func(b *testing.B) {
			book, err := ledger.Open(b.TempDir(), ledger.Options{})
			if err != nil {
				b.Fatal(err)
			}
			defer book.Close()
			output, err := json.Marshal(recoveredOutput{Result: turn.Result{Text: strings.Repeat("x", fixture.bodyBytes), Injected: &turn.Injected{Project: "project"}}})
			if err != nil {
				b.Fatal(err)
			}
			if _, err := book.DB().Exec(`WITH RECURSIVE n(seq) AS (VALUES(1) UNION ALL SELECT seq+1 FROM n WHERE seq<?)
 INSERT INTO commands(id,kind,actor,received_at,finished_at,result,error)
 SELECT printf('input-%08d',seq),'gateway-input','owner','2026-09-21T00:00:00Z','2026-09-21T00:00:00Z',
 json_object('message',json_object('MessageID',printf('input-%08d',seq),'SenderOpenID','owner','ConversationID',printf('chat-%04d',seq % ?),'Text','original prompt')),'' FROM n`, fixture.turns, fixture.conversations); err != nil {
				b.Fatal(err)
			}
			if _, err := book.DB().Exec(`INSERT INTO commands(id,kind,actor,received_at,finished_at,result,error)
 SELECT id||'/dispatch','gateway-input-dispatch','owner',received_at,finished_at,?,'' FROM commands WHERE kind='gateway-input'`, string(output)); err != nil {
				b.Fatal(err)
			}
			if _, err := book.DB().Exec(`INSERT INTO commands(id,kind,actor,received_at,finished_at,result,error)
 SELECT id||'/reply','gateway-input-reply','owner',received_at,finished_at,json_object('command_id',id,'receipt','external'),'' FROM commands WHERE kind='gateway-input'`); err != nil {
				b.Fatal(err)
			}
			h := NewChannelHistory(book)
			b.Run("Contains", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					found, err := h.Contains(b.Context(), "chat-0000")
					if err != nil || !found {
						b.Fatalf("contains: %v %v", found, err)
					}
				}
			})
			b.Run("List200", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					page, err := h.List(b.Context(), "", 200)
					if err != nil || len(page.Conversations) != fixture.conversations {
						b.Fatalf("list: %d %v", len(page.Conversations), err)
					}
				}
			})
			b.Run("Read50", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					page, err := h.Read(b.Context(), "chat-0000", "", 50)
					if err != nil || len(page.Replies) != 100 {
						b.Fatalf("read: %d %v", len(page.Replies), err)
					}
				}
			})
		})
	}
}
