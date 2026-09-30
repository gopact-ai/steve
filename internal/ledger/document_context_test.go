package ledger

import (
	"context"
	"errors"
	"testing"
	"time"
)

type contextDocumentWriter interface {
	SaveContext(context.Context, []byte) error
}

type documentDeadlineReplicator struct {
	book             *Ledger
	stage            string
	entered, release chan struct{}
}

func (r *documentDeadlineReplicator) wait(ctx context.Context) error {
	close(r.entered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.release:
		return errors.New("released stalled proposal")
	}
}
func (r *documentDeadlineReplicator) Prepare(ctx context.Context) (ReplicaPosition, error) {
	if r.stage == "prepare" {
		return ReplicaPosition{}, r.wait(ctx)
	}
	version, err := r.book.ReplicaVersion()
	return ReplicaPosition{Version: version, CoordinatorEpoch: 1}, err
}
func (r *documentDeadlineReplicator) Propose(ctx context.Context, _ ReplicatedWrite) ([]byte, error) {
	return nil, r.wait(ctx)
}

func TestDocumentSaveContextCancelsConsensusBeforeCommit(t *testing.T) {
	for _, stage := range []string{"prepare", "propose"} {
		for _, mode := range []string{"cancel", "deadline"} {
			t.Run(stage+"/"+mode, func(t *testing.T) {
				book, err := Open(t.TempDir(), Options{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = book.Close() })
				doc := book.Document("test")
				if err := doc.Save([]byte("old")); err != nil {
					t.Fatal(err)
				}
				writer, ok := any(doc).(contextDocumentWriter)
				if !ok {
					t.Fatal("document cannot save with a context")
				}
				replica := &documentDeadlineReplicator{book: book, stage: stage, entered: make(chan struct{}), release: make(chan struct{})}
				if err := book.AttachReplication(replica); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				if mode == "deadline" {
					cancel()
					ctx, cancel = context.WithTimeout(t.Context(), 250*time.Millisecond)
				}
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- writer.SaveContext(ctx, []byte("new")) }()
				select {
				case <-replica.entered:
				case <-time.After(time.Second):
					cancel()
					close(replica.release)
					<-done
					t.Fatal("proposal was not reached")
				}
				if mode == "cancel" {
					cancel()
				}
				select {
				case err := <-done:
					if !errors.Is(err, ctx.Err()) {
						t.Fatalf("save = %v, want %v", err, ctx.Err())
					}
				case <-time.After(time.Second):
					close(replica.release)
					<-done
					t.Fatal("save ignored its cancelled context")
				}
				got, _, err := doc.Load()
				if err != nil || string(got) != "old" {
					t.Fatalf("cancelled write changed document: %q, %v", got, err)
				}
				if !book.writerMu.TryLock() {
					t.Fatal("cancelled save retained the writer lock")
				}
				book.writerMu.Unlock()
			})
		}
	}
}

func TestDocumentSaveContextDoesNotWaitForAnotherWriter(t *testing.T) {
	book, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	writer, ok := any(book.Document("test")).(contextDocumentWriter)
	if !ok {
		t.Fatal("document cannot save with a context")
	}
	book.writerMu.Lock()
	done := make(chan error, 1)
	go func() { done <- writer.SaveContext(t.Context(), []byte("new")) }()
	select {
	case err := <-done:
		book.writerMu.Unlock()
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("busy writer: %v", err)
		}
	case <-time.After(time.Second):
		book.writerMu.Unlock()
		<-done
		t.Fatal("context document save waited for the busy writer")
	}
}
