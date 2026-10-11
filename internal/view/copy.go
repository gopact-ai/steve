package view

import "strings"

type Copy struct {
	Title          string
	Running        string
	Completed      string
	Failed         string
	Cancelled      string
	EarlierTools   string
	Execution      string
	Plan           string
	EarlierSteps   string
	Input          string
	Output         string
	Context        string
	In             string
	Out            string
	Hit            string
	Write          string
	Awaiting       string
	ApprovalTitle  string
	ApprovalTool   string
	ApprovalReason string
	ApprovalRule   string
	QuestionTitle  string
	QuestionHint   string
	Recover        string
	SentTo         string
	AllowOnce      string
	Deny           string
	Waking         string
	Finishing      string
	Saving         string
	Partial        string
	Stop           string
	Retry          string
}

// SettingsLine is a bounded identity label, independent of any card schema.
func SettingsLine(s Settings) string {
	parts := []string{}
	for _, value := range []string{s.Harness, s.Model, s.Mode} {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		runes := []rune(value)
		if len(runes) > 32 {
			value = string(runes[:31]) + "…"
		}
		parts = append(parts, value)
	}
	return strings.Join(parts, " · ")
}
