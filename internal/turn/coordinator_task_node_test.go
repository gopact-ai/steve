package turn

import (
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func TestTaskExecutionNodeSurvivesCoordinatorChangeAndReload(t *testing.T) {
	for _, selectedNode := range []string{"worker", ""} {
		t.Run("execution="+selectedNode, func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			store, err := task.OpenLedger(book, "")
			if err != nil {
				t.Fatal(err)
			}
			selected := agent.Agent{ID: "worker-agent", Node: selectedNode}
			req := Request{ConversationID: "chat"}
			var taskID string
			var nodes []string
			for _, coordinatorNode := range []string{"coordinator-a", "coordinator-b"} {
				c := New(nil, nil, nil, nil, 0)
				c.SetTasks(store, coordinatorNode)
				id, err := c.beginTask(req, selected, "continue work", project.Binding{}, "/workspace")
				if err != nil || id == "" {
					t.Fatalf("begin task: id=%q err=%v", id, err)
				}
				if taskID != "" && taskID != id {
					t.Fatal("coordinator change created another task")
				}
				taskID = id
				wantNode := selectedNode
				if wantNode == "" {
					wantNode = coordinatorNode
				}
				nodes = append(nodes, wantNode)
				tracked, _ := store.Get(id)
				if tracked.Node != wantNode || tracked.Attempts[len(tracked.Attempts)-1].Node != wantNode {
					t.Fatalf("task and attempt must name execution node %q, got task=%q attempt=%q", wantNode, tracked.Node, tracked.Attempts[len(tracked.Attempts)-1].Node)
				}
				if _, err := store.Finish(id, task.OutcomeOK, task.Tokens{}, 0); err != nil {
					t.Fatal(err)
				}
				store, err = task.OpenLedger(book, "")
				if err != nil {
					t.Fatal(err)
				}
				tracked, _ = store.Get(id)
				if tracked.Node != wantNode || len(tracked.Attempts) != len(nodes) {
					t.Fatal("execution location or attempt history changed after reload")
				}
				for i, node := range nodes {
					if tracked.Attempts[i].Node != node {
						t.Fatalf("attempt %d execution location changed after reload", i)
					}
				}
			}
		})
	}
}
