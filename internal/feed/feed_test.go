package feed

import (
	"errors"
	"testing"
)

func TestAfterResumesFromTheCursorAndRefusesWhatItNoLongerHolds(t *testing.T) {
	f := New("e1", 3)
	start := f.Head()
	for i := range 5 {
		if _, err := f.Publish("n", i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.After(start); !errors.Is(err, ErrCursorTooOld) {
		t.Fatalf("cursor before the window = %v", err)
	}
	got, err := f.After(Cursor{Epoch: "e1", Seq: 3})
	if err != nil || len(got) != 2 || got[0].Seq != 4 || got[1].Seq != 5 || string(got[1].Data) != "4" {
		t.Fatalf("after 3 = %+v, %v", got, err)
	}
	if got, err := f.After(f.Head()); err != nil || len(got) != 0 {
		t.Fatalf("at head = %+v, %v", got, err)
	}
	for _, c := range []Cursor{{Epoch: "e0", Seq: 4}, {Epoch: "e1", Seq: 9}} {
		if _, err := f.After(c); !errors.Is(err, ErrCursorTooOld) {
			t.Fatalf("foreign cursor %+v = %v", c, err)
		}
	}
}

func TestEmptyFeedHeadIsCurrent(t *testing.T) {
	f := New("e1", 0)
	if got, err := f.After(f.Head()); err != nil || len(got) != 0 {
		t.Fatalf("empty feed = %+v, %v", got, err)
	}
	f.Publish("n", 1)
	if got, err := f.After(Cursor{Epoch: "e1", Seq: 0}); err != nil || len(got) != 1 {
		t.Fatalf("first event = %+v, %v", got, err)
	}
}

func TestSubscribersAreWokenAndCanLeave(t *testing.T) {
	f := New("e1", 0)
	ch, stop := f.Subscribe()
	f.Publish("n", 1)
	f.Publish("n", 2)
	select {
	case <-ch:
	default:
		t.Fatal("subscriber not woken")
	}
	stop()
	f.Publish("n", 3)
	select {
	case <-ch:
		t.Fatal("stopped subscriber woken")
	default:
	}
}
