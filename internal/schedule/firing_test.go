package schedule

import (
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func firingStore(t *testing.T, channel string) (*Store, Job, time.Time, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schedules.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := at(t, "2026-08-28 09:30")
	s.now = func() time.Time { return now }
	j, err := s.Create(Job{Channel: channel, ConversationID: "console:work", ProjectID: "p", Member: "builder", Requester: "owner", Prompt: "work", AnchorMessage: "anchor", Spec: Spec{Kind: KindOnce, At: now.Add(time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	return s, j, now.Add(time.Minute), path
}

func TestDuePersistsFiringWithoutConsumingTheJob(t *testing.T) {
	s, job, now, path := firingStore(t, "console")
	due, err := s.Due(now)
	if err != nil || len(due) != 1 || due[0].Key == "" || !due[0].ScheduledAt.Equal(job.NextAt) {
		t.Fatalf("due=%+v err=%v", due, err)
	}
	stored, ok := s.Get(job.ID)
	if !ok || stored.Runs != 0 || !stored.NextAt.Equal(job.NextAt) {
		t.Fatalf("claim consumed job: %+v", stored)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := reopened.Due(now.Add(time.Second))
	if err != nil || len(again) != 1 || again[0].Key != due[0].Key {
		t.Fatalf("restart lost pending firing: %+v %v", again, err)
	}
	if err := reopened.BeginFiring(again[0].Key); err != nil {
		t.Fatal(err)
	}
	if err := reopened.AcceptFiring(again[0].Key, "exchange-id", now); err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Get(job.ID); ok {
		t.Fatal("accepted one-shot was not retired")
	}
	if err := reopened.AcceptFiring(again[0].Key, "exchange-id", now); err != nil {
		t.Fatalf("same receipt replay failed: %v", err)
	}
	if due, err := reopened.Due(now.Add(time.Hour)); err != nil || len(due) != 0 {
		t.Fatalf("accepted firing repeated: %+v %v", due, err)
	}
}

func TestFiringDispatchIsExclusive(t *testing.T) {
	s, _, now, _ := firingStore(t, "console")
	due, _ := s.Due(now)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if s.BeginFiring(due[0].Key) == nil {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("dispatch accepted %d writers", wins.Load())
	}
	if next, err := s.Due(now); err != nil || len(next) != 0 {
		t.Fatalf("live firing was repeated: %+v %v", next, err)
	}
}

func TestRestartRetriesConsoleButRequiresDecisionForFeishu(t *testing.T) {
	for _, channel := range []string{"console", "feishu"} {
		t.Run(channel, func(t *testing.T) {
			s, job, now, path := firingStore(t, channel)
			due, _ := s.Due(now)
			if err := s.BeginFiring(due[0].Key); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			again, err := reopened.Due(now)
			if err != nil {
				t.Fatal(err)
			}
			if channel == "console" {
				if len(again) != 1 || again[0].Key != due[0].Key {
					t.Fatalf("console lost safe replay: %+v", again)
				}
				return
			}
			if len(again) != 0 {
				t.Fatal("unknown IM operation retried")
			}
			stored, ok := reopened.Get(job.ID)
			if !ok || stored.State != FiringUnknown || stored.Error == "" || stored.PendingKey != due[0].Key {
				t.Fatalf("unknown firing not visible: %+v", stored)
			}
			if err := reopened.ResolveFiring(job.ID, "retry", "owner"); err != nil {
				t.Fatal(err)
			}
			again, err = reopened.Due(now)
			if err != nil || len(again) != 1 || again[0].Key != due[0].Key {
				t.Fatalf("explicit retry changed firing: %+v %v", again, err)
			}
		})
	}
}

func TestUnknownFiringCanBeConfirmedAndFailuresRemainRetryable(t *testing.T) {
	s, job, now, _ := firingStore(t, "feishu")
	due, _ := s.Due(now)
	if err := s.BeginFiring(due[0].Key); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveFiring(job.ID, "confirm", "owner"); err == nil {
		t.Fatal("active dispatch could be resolved")
	}
	if err := s.FailFiring(due[0].Key, errors.New("provider refused"), false); err != nil {
		t.Fatal(err)
	}
	again, _ := s.Due(now)
	if len(again) != 1 || again[0].Error == "" {
		t.Fatalf("definite failure disappeared: %+v", again)
	}
	if err := s.BeginFiring(due[0].Key); err != nil {
		t.Fatal(err)
	}
	if err := s.FailFiring(due[0].Key, errors.New("response lost"), true); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.Due(now); len(again) != 0 {
		t.Fatal("unknown firing retried")
	}
	if err := s.ResolveFiring(job.ID, "confirm", "owner"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(job.ID); ok {
		t.Fatal("confirmed one-shot stayed pending")
	}
}

func TestDelayedAcceptanceDoesNotShiftTheClaimedSchedule(t *testing.T) {
	for _, manual := range []bool{false, true} {
		s, _, now, path := firingStore(t, "feishu")
		for _, job := range s.List("") {
			if _, _, err := s.Delete(job.ID); err != nil {
				t.Fatal(err)
			}
		}
		job, err := s.Create(Job{Channel: "feishu", ConversationID: "chat", Prompt: "work", Spec: Spec{Kind: KindEvery, Every: 10 * time.Minute}})
		if err != nil {
			t.Fatal(err)
		}
		claimAt := job.NextAt.Add(time.Minute)
		due, err := s.Due(claimAt)
		if err != nil || len(due) != 1 {
			t.Fatalf("due=%+v %v", due, err)
		}
		following := claimAt.Add(10 * time.Minute)
		if !due[0].FollowingAt.Equal(following) {
			t.Fatalf("following=%s expected=%s", due[0].FollowingAt, following)
		}
		if err := s.BeginFiring(due[0].Key); err != nil {
			t.Fatal(err)
		}
		if manual {
			restarted, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			restarted.now = func() time.Time { return now.Add(8 * time.Hour) }
			if err := restarted.ResolveFiring(job.ID, "confirm", "owner"); err != nil {
				t.Fatal(err)
			}
			s = restarted
		} else {
			if err := s.AcceptFiring(due[0].Key, "receipt", now.Add(8*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		stored, ok := s.Get(job.ID)
		if !ok || !stored.NextAt.Equal(following) || stored.Runs != 1 {
			t.Fatalf("late acceptance shifted schedule: %+v", stored)
		}
	}
}
