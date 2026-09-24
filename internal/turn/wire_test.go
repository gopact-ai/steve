package turn

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// unwired builds a coordinator through New without handing it callbacks.
func unwired(t *testing.T) *Coordinator {
	t.Helper()
	var deps Deps
	fillDeps(t, testLedger(t), &deps)
	c, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// wirePanic is what Wire panicked with, or nil.
func wirePanic(c *Coordinator, callbacks Callbacks) (recovered any) {
	defer func() { recovered = recover() }()
	c.Wire(callbacks)
	return nil
}

func TestWireHandsOverEveryCallback(t *testing.T) {
	c := unwired(t)
	callbacks := fillCallbacks(Callbacks{AgentGate: &fakeGate{}, AfterTurn: func(string) {},
		TurnPreface: func(context.Context, string) Preface { return Preface{} }})
	c.Wire(callbacks)
	if c.supervisor != callbacks.Supervisor || c.gate != callbacks.AgentGate || c.attach == nil || c.afterTurn == nil ||
		c.turnPreface == nil || c.notifier == nil || c.resumer == nil || c.resumeDispatcher == nil {
		t.Fatal("Wire dropped a callback")
	}
}

func TestWireTwicePanics(t *testing.T) {
	c := unwired(t)
	c.Wire(fillCallbacks(Callbacks{}))
	if got := wirePanic(c, fillCallbacks(Callbacks{})); got == nil {
		t.Fatal("a second Wire replaced the callbacks the coordinator already reads")
	}
}

// Each callback Callbacks.required lists is refused alone, and the panic
// names it.
func TestWireRefusesEachRequiredCallbackAlone(t *testing.T) {
	full := fillCallbacks(Callbacks{})
	required := full.required()
	if len(required) == 0 {
		t.Fatal("Callbacks.required lists no callback")
	}
	for _, callback := range required {
		t.Run(callback.name, func(t *testing.T) {
			field, ok := reflect.TypeFor[Callbacks]().FieldByName(callback.name)
			if !ok {
				t.Fatalf("Callbacks.required lists %s, which is no Callbacks field", callback.name)
			}
			without := full
			reflect.ValueOf(&without).Elem().FieldByIndex(field.Index).SetZero()
			got := wirePanic(unwired(t), without)
			if want := "turn: missing callbacks: " + callback.name; fmt.Sprint(got) != want {
				t.Fatalf("Wire without %s panicked with %v, want %q", callback.name, got, want)
			}
		})
	}
	if got := wirePanic(unwired(t), Callbacks{}); !strings.Contains(fmt.Sprint(got), "missing callbacks") {
		t.Fatalf("Wire with nothing panicked with %v", got)
	}
}
