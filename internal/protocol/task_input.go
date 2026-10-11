package protocol

import "strings"

// TaskVerb is what the user asked to do to a task.
type TaskVerb int

// taskVerbs maps chat words to verbs. The Chinese aliases are not a nicety:
// the surface is Chinese by default, and a verb that only answers to English
// would make half of it untypable.
var taskVerbs = map[string]TaskVerb{
	"pause": TaskPause, "暂停": TaskPause,
	"resume": TaskResume, "继续": TaskResume, "恢复": TaskResume,
	"cancel": TaskCancel, "取消": TaskCancel, "结束": TaskCancel,
	"complete": TaskComplete, "done": TaskComplete, "完成": TaskComplete,
	"show": TaskShow, "详情": TaskShow,
	"handled": TaskHandled, "handle": TaskHandled, "已处理": TaskHandled, "人工处理": TaskHandled, "手动处理": TaskHandled,
	"ignore": TaskIgnored, "ignored": TaskIgnored, "忽略": TaskIgnored, "无需关注": TaskIgnored,
	"reopen": TaskReopen, "重开": TaskReopen, "重新打开": TaskReopen,
}

// ParseTaskArgs reads "pause 12", "12 pause", "12" or "pause", so nobody has
// to remember which half comes first. An unrecognised word is a usage error
// rather than a guess: acting on the wrong task is worse than asking again.
func ParseTaskArgs(rest string) (TaskVerb, string, bool) {
	verb, id := TaskList, ""
	for _, field := range strings.Fields(rest) {
		if trimmed := strings.TrimPrefix(field, "#"); IsTaskID(trimmed) {
			id = trimmed
			if verb == TaskList {
				verb = TaskShow
			}
			continue
		}
		found, ok := taskVerbs[strings.ToLower(field)]
		if !ok {
			return TaskList, "", false
		}
		verb = found
	}
	return verb, id, true
}

func IsTaskID(s string) bool {
	if prefix, tail, ok := strings.Cut(s, "~"); ok {
		if len(prefix) != 13 || prefix[0] != 'h' {
			return false
		}
		for _, r := range prefix[1:] {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
				return false
			}
		}
		s = tail
	}
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

const (
	TaskList TaskVerb = iota
	TaskShow
	TaskPause
	TaskResume
	TaskCancel
	TaskComplete
	// TaskHandled and TaskIgnored are the two ways a person closes a
	// failed task without pretending it was called off; TaskReopen puts
	// it back in front of them.
	TaskHandled
	TaskIgnored
	TaskReopen
)
