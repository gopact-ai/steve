package contentreplica_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/contentreplica"
)

// A placement the policy could not check for now — its committed state out
// of reach, its replica behind — refuses nothing, wherever the check is
// made: the error says the check was unavailable, not that the placement
// was refused, so a caller asks again instead of giving the content up.
func TestAPlacementThatCouldNotBeCheckedIsNotARefusal(t *testing.T) {
	data := []byte("asked again later")
	ref := checkpoint.Reference(data)
	prepare := func(c *contentreplica.Client) (contentreplica.Manifest, error) {
		return c.Prepare(t.Context(), "p", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
	}
	cases := []struct {
		name string
		run  func(p *places, r *transport, client func(string) *contentreplica.Client) error
	}{
		{name: "own node", run: func(p *places, _ *transport, client func(string) *contentreplica.Client) error {
			p.unsure["a"] = true
			_, err := client("a").CheckLocal(t.Context(), "p")
			return err
		}},
		{name: "second copy's node", run: func(p *places, _ *transport, client func(string) *contentreplica.Client) error {
			p.unsure["b"], p.unsure["c"] = true, true
			_, err := prepare(client("a"))
			return err
		}},
		{name: "receiver's own check", run: func(p *places, r *transport, client func(string) *contentreplica.Client) error {
			r.before = func(node string, _ contentreplica.Object) { p.unsure[node] = true }
			_, err := prepare(client("a"))
			return err
		}},
		{name: "own node again once the copies are made", run: func(p *places, r *transport, client func(string) *contentreplica.Client) error {
			r.before = func(node string, _ contentreplica.Object) {
				if node == "b" {
					p.unsure["a"] = true
				}
			}
			_, err := prepare(client("a"))
			return err
		}},
		{name: "reading node", run: func(p *places, _ *transport, client func(string) *contentreplica.Client) error {
			m, err := prepare(client("a"))
			if err != nil {
				t.Fatal(err)
			}
			p.unsure["c"] = true
			_, err = client("c").Read(t.Context(), m, &bytes.Buffer{})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, r, client := newCluster(t, openBook(t), "internal", "a", "b", "c")
			err := c.run(p, r, client)
			if !errors.Is(err, contentreplica.ErrUnavailable) || errors.Is(err, contentreplica.ErrPlacement) || errors.Is(err, checkpoint.ErrPlacement) {
				t.Fatalf("unchecked placement: %v, want unavailable and not refused", err)
			}
		})
	}
}
