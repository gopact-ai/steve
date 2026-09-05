package nodewire

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ResumeAck is the first payload on a resumed stream. The reserved prefix
// cannot be mistaken for ACP JSON; the transport consumes it before ACP reads.
// Wire encoding: steve-resume {"have_in":N}\n. It does not advance AfterOut.
type ResumeAck struct {
	HaveIn uint64 `json:"have_in"`
}

const resumePrefix = "steve-resume "

func (a ResumeAck) Write(w io.Writer) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s%s\n", resumePrefix, b)
	return err
}

func ReadResumeAck(r *bufio.Reader) (ResumeAck, error) {
	var ack ResumeAck
	line, err := r.ReadString('\n')
	if err != nil {
		return ack, err
	}
	if !strings.HasPrefix(line, resumePrefix) {
		return ack, fmt.Errorf("nodewire: missing resume acknowledgement")
	}
	err = json.Unmarshal([]byte(strings.TrimPrefix(line, resumePrefix)), &ack)
	return ack, err
}
