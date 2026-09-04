package skills

import (
	"strings"
	"unicode"
)

type Action int

const (
	ActionUnknown Action = iota
	ActionList
	ActionHelp
	ActionEnable
	ActionDisable
	ActionPathAdd
	ActionPathRemove
	// ActionSourceAdd installs a git source; ActionUpdate fetches every
	// source again.
	ActionSourceAdd
	ActionUpdate
)

const (
	TokenEnable  = "enable"
	TokenDisable = "disable"
	TokenPath    = "path"
	TokenAdd     = "add"
	TokenRm      = "rm"
	TokenRemove  = "remove"
	TokenHelp    = "help"
	TokenAddSrc  = "add"
	TokenUpdate  = "update"
)

func ParseAction(rest string) (Action, string) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return ActionList, ""
	}
	verb, arg := splitCmd(rest)
	switch verb {
	case TokenEnable:
		return ActionEnable, arg
	case TokenDisable:
		return ActionDisable, arg
	case TokenHelp:
		return ActionHelp, arg
	case TokenAddSrc:
		return ActionSourceAdd, arg
	case TokenUpdate:
		return ActionUpdate, arg
	case TokenPath:
		sub, path := splitCmd(arg)
		switch sub {
		case TokenAdd:
			return ActionPathAdd, path
		case TokenRm, TokenRemove:
			return ActionPathRemove, path
		default:
			return ActionUnknown, ""
		}
	default:
		return ActionUnknown, ""
	}
}

func splitCmd(value string) (string, string) {
	value = strings.TrimSpace(value)
	index := strings.IndexFunc(value, unicode.IsSpace)
	if index < 0 {
		return value, ""
	}
	return value[:index], strings.TrimSpace(value[index:])
}
