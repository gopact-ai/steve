// Package schedule turns "every morning at nine" into something the gateway
// can fire, and remembers it across restarts. A scheduled run is not a special
// kind of turn: it is an ordinary message Steve sends on the user's behalf, so
// everything downstream — cards, tasks, budgets, delivery — already works.
package schedule

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Kind string

const (
	// KindEvery repeats on a fixed interval from the last run.
	KindEvery Kind = "every"
	// KindDaily repeats at a wall-clock time, optionally on one weekday.
	KindDaily Kind = "daily"
	// KindOnce fires at one moment and is then forgotten.
	KindOnce Kind = "once"
)

// anyDay marks a daily spec with no weekday constraint.
const anyDay = -1

// Spec is when a job runs. It is deliberately smaller than cron: the shapes
// people actually ask for in a chat are "every N minutes", "every day at
// nine", "every Monday at nine" and "at half past". Anything more exotic is
// better expressed as a task that schedules itself.
type Spec struct {
	Kind    Kind          `json:"kind"`
	Every   time.Duration `json:"every,omitempty"`
	Hour    int           `json:"hour,omitempty"`
	Minute  int           `json:"minute,omitempty"`
	Weekday int           `json:"weekday,omitempty"`
	At      time.Time     `json:"at,omitzero"`
	// Text is what the user typed, kept verbatim so the listing answers in
	// their own words rather than in a normalised form they never wrote.
	Text string `json:"text"`
}

// MinEvery bounds the interval. A schedule firing faster than this is not a
// reminder, it is a loop with a chat window attached.
const MinEvery = time.Minute

// Next reports the first firing strictly after the given moment. A zero time
// means the job is spent, which is how one-shots retire.
func (s Spec) Next(after time.Time) time.Time {
	switch s.Kind {
	case KindEvery:
		if s.Every < MinEvery {
			return time.Time{}
		}
		return after.Add(s.Every)
	case KindDaily:
		candidate := time.Date(after.Year(), after.Month(), after.Day(), s.Hour, s.Minute, 0, 0, after.Location())
		for i := 0; i < 8; i++ {
			if candidate.After(after) && (s.Weekday == anyDay || int(candidate.Weekday()) == s.Weekday) {
				return candidate
			}
			candidate = candidate.AddDate(0, 0, 1)
		}
		return time.Time{}
	case KindOnce:
		if s.At.After(after) {
			return s.At
		}
		return time.Time{}
	}
	return time.Time{}
}

func (s Spec) Recurring() bool { return s.Kind == KindEvery || s.Kind == KindDaily }

// ParseEvery reads the recurring half of the surface: "30m", "2小时",
// "09:00", "day 09:00", "周一 09:00". It returns the prompt that follows,
// because where the spec ends and the instruction begins is exactly the
// question a chat command has to answer for itself.
func ParseEvery(input string, now time.Time) (Spec, string, error) {
	return parse(input, now, everySpec)
}

// ParseAt reads the one-shot half: "09:00", "明天 09:00", "30m",
// "2026-08-29 09:00".
func ParseAt(input string, now time.Time) (Spec, string, error) {
	return parse(input, now, onceSpec)
}

// parse takes the longest leading run of words that forms a valid spec, up to
// two. Longest-first matters: "every 周一 09:00" must not stop at "周一".
func parse(input string, now time.Time, build func([]string, time.Time) (Spec, bool)) (Spec, string, error) {
	fields := strings.Fields(strings.TrimSpace(input))
	if len(fields) == 0 {
		return Spec{}, "", fmt.Errorf("empty schedule")
	}
	for width := min(2, len(fields)); width >= 1; width-- {
		spec, ok := build(fields[:width], now)
		if !ok {
			continue
		}
		prompt := strings.TrimSpace(strings.Join(fields[width:], " "))
		if prompt == "" {
			return Spec{}, "", fmt.Errorf("nothing to run")
		}
		spec.Text = strings.Join(fields[:width], " ")
		return spec, prompt, nil
	}
	return Spec{}, "", fmt.Errorf("unrecognised schedule %q", fields[0])
}

func everySpec(words []string, now time.Time) (Spec, bool) {
	switch len(words) {
	case 1:
		if d, ok := parseDuration(words[0]); ok {
			if d < MinEvery {
				return Spec{}, false
			}
			return Spec{Kind: KindEvery, Every: d}, true
		}
		if hour, minute, ok := parseClock(words[0]); ok {
			return Spec{Kind: KindDaily, Hour: hour, Minute: minute, Weekday: anyDay}, true
		}
	case 2:
		hour, minute, ok := parseClock(words[1])
		if !ok {
			return Spec{}, false
		}
		if isEveryDayWord(words[0]) {
			return Spec{Kind: KindDaily, Hour: hour, Minute: minute, Weekday: anyDay}, true
		}
		if day, ok := parseWeekday(words[0]); ok {
			return Spec{Kind: KindDaily, Hour: hour, Minute: minute, Weekday: int(day)}, true
		}
	}
	return Spec{}, false
}

func onceSpec(words []string, now time.Time) (Spec, bool) {
	switch len(words) {
	case 1:
		if d, ok := parseDuration(words[0]); ok {
			return Spec{Kind: KindOnce, At: now.Add(d)}, true
		}
		if hour, minute, ok := parseClock(words[0]); ok {
			return Spec{Kind: KindOnce, At: nextClock(now, hour, minute, 0)}, true
		}
	case 2:
		if hour, minute, ok := parseClock(words[1]); ok {
			if isTomorrowWord(words[0]) {
				return Spec{Kind: KindOnce, At: nextClock(now, hour, minute, 1)}, true
			}
			if day, ok := parseWeekday(words[0]); ok {
				return Spec{Kind: KindOnce, At: nextWeekdayClock(now, day, hour, minute)}, true
			}
			if date, ok := parseDate(words[0], now.Location()); ok {
				return Spec{Kind: KindOnce, At: time.Date(
					date.Year(), date.Month(), date.Day(), hour, minute, 0, 0, now.Location())}, true
			}
		}
	}
	return Spec{}, false
}

// nextClock is today's hour:minute if it is still ahead, tomorrow's otherwise.
// skip forces a number of days forward, which is what "tomorrow" means.
func nextClock(now time.Time, hour, minute, skip int) time.Time {
	candidate := time.Date(now.Year(), now.Month(), now.Day()+skip, hour, minute, 0, 0, now.Location())
	if candidate.After(now) {
		return candidate
	}
	return candidate.AddDate(0, 0, 1)
}

func nextWeekdayClock(now time.Time, day time.Weekday, hour, minute int) time.Time {
	candidate := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	for i := 0; i < 8; i++ {
		if candidate.After(now) && candidate.Weekday() == day {
			return candidate
		}
		candidate = candidate.AddDate(0, 0, 1)
	}
	return time.Time{}
}

// chineseDuration matches "30分钟", "2小时", "1天" — the units people type in
// the chat this thing lives in.
var chineseDuration = regexp.MustCompile(`^(\d+)\s*(秒|分钟|分|小时|时|天|周)$`)

func parseDuration(word string) (time.Duration, bool) {
	if d, err := time.ParseDuration(word); err == nil && d > 0 {
		return d, true
	}
	match := chineseDuration.FindStringSubmatch(word)
	if match == nil {
		return 0, false
	}
	n, err := strconv.Atoi(match[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	unit := map[string]time.Duration{
		"秒": time.Second, "分钟": time.Minute, "分": time.Minute,
		"小时": time.Hour, "时": time.Hour, "天": 24 * time.Hour, "周": 7 * 24 * time.Hour,
	}[match[2]]
	return time.Duration(n) * unit, true
}

var clock = regexp.MustCompile(`^(\d{1,2})([:：点])(\d{2})?$`)

func parseClock(word string) (int, int, bool) {
	match := clock.FindStringSubmatch(word)
	if match == nil {
		return 0, 0, false
	}
	hour, err := strconv.Atoi(match[1])
	if err != nil || hour > 23 {
		return 0, 0, false
	}
	// "9点" is a whole hour; "9:" is a time somebody stopped typing. Only
	// the first may leave the minutes out — a spec fires when nobody is
	// watching, so a half-written one has to be refused, not completed.
	if match[3] == "" {
		return hour, 0, match[2] == "点"
	}
	minute, err := strconv.Atoi(match[3])
	if err != nil || minute > 59 {
		return 0, 0, false
	}
	return hour, minute, true
}

var date = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)

func parseDate(word string, loc *time.Location) (time.Time, bool) {
	match := date.FindStringSubmatch(word)
	if match == nil {
		return time.Time{}, false
	}
	parsed, err := time.ParseInLocation("2006-01-02", match[0], loc)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday, "周日": time.Sunday, "周天": time.Sunday, "星期日": time.Sunday,
	"mon": time.Monday, "monday": time.Monday, "周一": time.Monday, "星期一": time.Monday,
	"tue": time.Tuesday, "tuesday": time.Tuesday, "周二": time.Tuesday, "星期二": time.Tuesday,
	"wed": time.Wednesday, "wednesday": time.Wednesday, "周三": time.Wednesday, "星期三": time.Wednesday,
	"thu": time.Thursday, "thursday": time.Thursday, "周四": time.Thursday, "星期四": time.Thursday,
	"fri": time.Friday, "friday": time.Friday, "周五": time.Friday, "星期五": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday, "周六": time.Saturday, "星期六": time.Saturday,
}

func parseWeekday(word string) (time.Weekday, bool) {
	day, ok := weekdays[strings.ToLower(word)]
	return day, ok
}

func isEveryDayWord(word string) bool {
	switch strings.ToLower(word) {
	case "day", "daily", "每天", "天", "每日":
		return true
	}
	return false
}

func isTomorrowWord(word string) bool {
	switch strings.ToLower(word) {
	case "tomorrow", "明天", "明日":
		return true
	}
	return false
}
