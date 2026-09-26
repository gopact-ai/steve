package i18n

import (
	"regexp"
	"slices"
	"strconv"
	"testing"
)

// directive matches one fmt verb, with an optional explicit argument index.
var directive = regexp.MustCompile(`%(?:\[(\d+)\])?[-+# 0]*(?:\d+|\*)?(?:\.(?:\d+|\*)?)?([a-zA-Z%])`)

// arguments lists each argument tmpl formats, marking one it wraps as an
// error: "2w" wraps the second argument, "1" formats the first.
func arguments(tmpl string) []string {
	var used []string
	next := 1
	for _, m := range directive.FindAllStringSubmatch(tmpl, -1) {
		if m[2] == "%" {
			continue
		}
		if m[1] != "" {
			next, _ = strconv.Atoi(m[1])
		}
		arg := strconv.Itoa(next)
		if m[2] == "w" {
			arg += "w"
		}
		used = append(used, arg)
		next++
	}
	slices.Sort(used)
	return slices.Compact(used)
}

// TestTranslationsTakeTheSameArguments keeps every language's entry
// formatting the same arguments, so a caller's values
// land in each translation, none is left over as %!(EXTRA ...), and an
// error wrapped in one language is wrapped in all.
func TestTranslationsTakeTheSameArguments(t *testing.T) {
	for key, zhText := range zh {
		enText, ok := en[key]
		if !ok {
			continue
		}
		if a, b := arguments(zhText), arguments(enText); !slices.Equal(a, b) {
			t.Errorf("%s: zh formats arguments %v, en %v", key, a, b)
		}
	}
}
