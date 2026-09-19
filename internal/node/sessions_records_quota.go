package node

import (
	"fmt"
	"strings"
)

// SQLite maintains usage from the actual changed rows, including cold and
// uncertain evidence. A failed limit check rolls back the entire transition;
// omitting historical rows from a hot record never frees their quota.
func sessionRecordQuotaSchema() []string {
	var statements []string
	for _, row := range []struct {
		table, count string
		fields       []string
	}{
		{"session_progress", "", []string{"progress"}},
		{"session_commands", "command_count", []string{"binding", "fingerprint", "command", "progress"}},
		{"session_questions", "question_count", []string{"question"}},
	} {
		size := func(ref string) string {
			parts := make([]string, len(row.fields))
			for i, field := range row.fields {
				// fingerprint is TEXT; count its encoded bytes, not characters.
				parts[i] = "length(CAST(" + ref + "." + field + " AS BLOB))"
			}
			return strings.Join(parts, "+")
		}
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			ref, delta, counter := "NEW", "+("+size("NEW")+")", ""
			if event == "DELETE" {
				ref, delta = "OLD", "-("+size("OLD")+")"
			} else if event == "UPDATE" {
				delta += "-(" + size("OLD") + ")"
			}
			if row.count != "" && event != "UPDATE" {
				sign := "+1"
				if event == "DELETE" {
					sign = "-1"
				}
				counter = ", " + row.count + "=" + row.count + sign
			}
			statement := fmt.Sprintf(`CREATE TRIGGER %s_quota_%s AFTER %s ON %s BEGIN
				UPDATE sessions SET retained_bytes=retained_bytes%s%s WHERE id=%s.session_id; END`,
				row.table, strings.ToLower(event), event, row.table, delta, counter, ref)
			statements = append(statements, statement)
		}
	}
	return statements
}
