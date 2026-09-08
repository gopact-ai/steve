package app

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestDoctorReportsGitVersionAndAdvisory(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	reportGit("hub h", nodewire.Advert{Git: "2.39.5", GitMinimum: "2.38"})
	reportGit("node n", nodewire.Advert{Git: "2.37.9"})
	for _, want := range []string{"hub h git=2.39.5, minimum=2.38", "node n git=2.37.9, minimum=2.38", "below the minimum 2.38"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("doctor output lacks %q: %s", want, &output)
		}
	}
	if strings.Contains(output.String(), "hub h warning:") {
		t.Fatalf("supported git warned: %s", &output)
	}
}
