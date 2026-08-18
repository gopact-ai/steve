package setup

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/gopact-ai/steve/internal/i18n"
	"golang.org/x/term"
)

func promptSelect(opts Options, in *bufio.Reader, out io.Writer, question string, options []option, initial string) (string, error) {
	if canUseRawKeys(opts) {
		return promptSelectKeys(opts, out, question, options, initial)
	}
	return promptSelectLine(opts, in, out, question, options, initial)
}

func canUseRawKeys(opts Options) bool {
	if opts.In != nil && opts.In != os.Stdin {
		return false
	}
	return term.IsTerminal(int(os.Stdin.Fd()))
}

func promptSelectLine(opts Options, in *bufio.Reader, out io.Writer, question string, options []option, initial string) (string, error) {
	fmt.Fprintf(out, "%s:\n", question)
	defaultIndex := 1
	for i, item := range options {
		mark := " "
		if item.value == initial {
			mark = ">"
			defaultIndex = i + 1
		}
		fmt.Fprintf(out, "  %s %d) %s\n", mark, i+1, item.label)
	}
	fmt.Fprintf(out, opts.Catalog.T(i18n.SetupEnterNumber), defaultIndex)
	line, err := in.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	value, ok := matchChoice(line, options, initial)
	if !ok {
		return "", fmt.Errorf("setup: unknown choice %q", strings.TrimSpace(line))
	}
	return value, nil
}

func promptSelectKeys(opts Options, out io.Writer, question string, options []option, initial string) (string, error) {
	fd := int(os.Stdin.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return "", fmt.Errorf("setup: read keyboard: %w", err)
	}
	defer term.Restore(fd, state)
	fmt.Fprint(out, "\033[?25l")
	defer fmt.Fprint(out, "\033[?25h")

	idx := 0
	for i, item := range options {
		if item.value == initial {
			idx = i
		}
	}
	fmt.Fprintf(out, "\r\n%s\r\n", question)
	drawn := drawSelectMenu(out, options, idx, opts.Catalog)

	finish := func(choice option) (string, error) {
		fmt.Fprintf(out, "\r\033[%dA\033[J%s\r\n", drawn, opts.Catalog.T(i18n.SetupSelected, choice.label))
		return choice.value, nil
	}

	for {
		key, err := readSelectKey(os.Stdin)
		if err != nil {
			fmt.Fprint(out, "\r\n")
			return "", err
		}
		switch key.kind {
		case selectKeyUp:
			if idx > 0 {
				idx--
			} else {
				idx = len(options) - 1
			}
		case selectKeyDown:
			if idx < len(options)-1 {
				idx++
			} else {
				idx = 0
			}
		case selectKeyEnter:
			return finish(options[idx])
		case selectKeyInterrupt:
			fmt.Fprint(out, "\r\n")
			return "", fmt.Errorf("setup: interrupted")
		case selectKeyDigit:
			n := key.digit - 1
			if n >= 0 && n < len(options) {
				return finish(options[n])
			}
			continue
		default:
			continue
		}
		// Cursor is on the blank line after the hint. Move back over the
		// menu we just drew, then wipe leftovers from earlier frames.
		fmt.Fprintf(out, "\r\033[%dA\033[J", drawn)
		drawn = drawSelectMenu(out, options, idx, opts.Catalog)
	}
}

func drawSelectMenu(out io.Writer, options []option, idx int, text i18n.Catalog) int {
	for i, item := range options {
		cursor := "  "
		if i == idx {
			cursor = "> "
		}
		fmt.Fprintf(out, "\r\033[2K%s%d) %s\r\n", cursor, i+1, item.label)
	}
	fmt.Fprintf(out, "\r\033[2K%s\r\n", text.T(i18n.SetupArrowHint))
	return len(options) + 1
}

type selectKeyKind int

const (
	selectKeyNone selectKeyKind = iota
	selectKeyUp
	selectKeyDown
	selectKeyEnter
	selectKeyInterrupt
	selectKeyDigit
)

type selectKey struct {
	kind  selectKeyKind
	digit int
}

func readSelectKey(r io.Reader) (selectKey, error) {
	var buf [3]byte
	if _, err := io.ReadFull(r, buf[:1]); err != nil {
		return selectKey{}, err
	}
	switch buf[0] {
	case 0x03:
		return selectKey{kind: selectKeyInterrupt}, nil
	case '\r', '\n':
		return selectKey{kind: selectKeyEnter}, nil
	case 'k', 'K':
		return selectKey{kind: selectKeyUp}, nil
	case 'j', 'J':
		return selectKey{kind: selectKeyDown}, nil
	case 0x1b:
		if _, err := io.ReadFull(r, buf[1:3]); err != nil {
			return selectKey{kind: selectKeyNone}, nil
		}
		if buf[1] == '[' {
			switch buf[2] {
			case 'A':
				return selectKey{kind: selectKeyUp}, nil
			case 'B':
				return selectKey{kind: selectKeyDown}, nil
			}
		}
		return selectKey{kind: selectKeyNone}, nil
	}
	if buf[0] >= '1' && buf[0] <= '9' {
		return selectKey{kind: selectKeyDigit, digit: int(buf[0] - '0')}, nil
	}
	if buf[0] >= 0xef {
		// UTF-8 fullwidth digit (EF BC 91 = １). Read the rest and map.
		rest := make([]byte, 2)
		if _, err := io.ReadFull(r, rest); err == nil {
			r := []rune(string(append([]byte{buf[0]}, rest...)))
			if len(r) == 1 && r[0] >= '１' && r[0] <= '９' {
				return selectKey{kind: selectKeyDigit, digit: int(r[0] - '１' + 1)}, nil
			}
		}
	}
	return selectKey{kind: selectKeyNone}, nil
}

func matchChoice(line string, options []option, initial string) (string, bool) {
	line = normalizeChoice(line)
	if line == "" {
		return initial, true
	}
	for i, item := range options {
		if line == item.value || line == strconv.Itoa(i+1) {
			return item.value, true
		}
	}
	return "", false
}

func normalizeChoice(value string) string {
	value = stripANSI(value)
	var mapped []rune
	for _, r := range value {
		switch {
		case r >= '０' && r <= '９':
			mapped = append(mapped, r-'０'+'0')
		case unicode.IsSpace(r):
			continue
		default:
			mapped = append(mapped, r)
		}
	}
	return strings.TrimRight(string(mapped), ".)、）")
}

func stripANSI(value string) string {
	var b strings.Builder
	i := 0
	for i < len(value) {
		if value[i] == 0x1b {
			i++
			if i < len(value) && value[i] == '[' {
				i++
				for i < len(value) && (value[i] < '@' || value[i] > '~') {
					i++
				}
				if i < len(value) {
					i++
				}
			}
			continue
		}
		b.WriteByte(value[i])
		i++
	}
	return b.String()
}
