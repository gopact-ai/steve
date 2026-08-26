// Package i18n formats user-visible Steve text.
package i18n

import (
	"fmt"
	"strings"

	"github.com/gopact-ai/steve/internal/config"
)

type Locale string

const (
	LocaleZH Locale = "zh"
	LocaleEN Locale = "en"
)

func FromDomain(domain string) Locale {
	if domain == config.DomainLark {
		return LocaleEN
	}
	return LocaleZH
}

func FromLang(lang string) Locale {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if cut, _, ok := strings.Cut(lang, "."); ok {
		lang = cut
	}
	if cut, _, ok := strings.Cut(lang, "_"); ok {
		lang = cut
	}
	if cut, _, ok := strings.Cut(lang, "-"); ok {
		lang = cut
	}
	if lang == "en" {
		return LocaleEN
	}
	return LocaleZH
}

type Key string

const (
	TurnCanceled           Key = "turn_canceled"
	AgentFailed            Key = "agent_failed"
	EmptyReply             Key = "empty_reply"
	Truncated              Key = "truncated"
	Switched               Key = "switched"
	SkillsUpdating         Key = "skills_updating"
	Tainted                Key = "tainted"
	CapabilityDrift        Key = "capability_drift"
	WorkspaceDrift         Key = "workspace_drift"
	StateSaveFailed        Key = "state_save_failed"
	Reset                  Key = "reset"
	NoRunningTurn          Key = "no_running_turn"
	TurnBusy               Key = "turn_busy"
	BudgetTurns            Key = "budget_turns"
	BudgetElapsed          Key = "budget_elapsed"
	CancelRequested        Key = "cancel_requested"
	SkillsOwnerOnly        Key = "skills_owner_only"
	SkillsUnconfigured     Key = "skills_unconfigured"
	SkillsEnabled          Key = "skills_enabled"
	SkillsDisabled         Key = "skills_disabled"
	SkillsPathAdded        Key = "skills_path_added"
	SkillsPathRemoved      Key = "skills_path_removed"
	SkillsBusy             Key = "skills_busy"
	SkillsInvalidEnabled   Key = "skills_invalid_enabled"
	SkillsInvalidAvailable Key = "skills_invalid_available"
	SkillsNone             Key = "skills_none"
	SkillsNoneOff          Key = "skills_none_off"
	SkillsSearchNone       Key = "skills_search_none"
	SkillsUsage            Key = "skills_usage"
	SetupEditHome          Key = "setup_edit_home"
	SetupCreatedConfig     Key = "setup_created_config"
	SetupBotIdentity       Key = "setup_bot_identity"
	SetupCreateAppFailed   Key = "setup_create_app_failed"
	SetupCreatedApp        Key = "setup_created_app"
	SetupOwnerResolveFail  Key = "setup_owner_resolve_fail"
	SetupAppSource         Key = "setup_app_source"
	SetupAppSourceCreate   Key = "setup_app_source_create"
	SetupAppSourceManual   Key = "setup_app_source_manual"
	SetupDomain            Key = "setup_domain"
	SetupDomainFeishu      Key = "setup_domain_feishu"
	SetupDomainLark        Key = "setup_domain_lark"
	SetupGroupPolicy       Key = "setup_group_policy"
	SetupGroupAllowlist    Key = "setup_group_allowlist"
	SetupGroupOpen         Key = "setup_group_open"
	SetupGroupDisabled     Key = "setup_group_disabled"
	SetupAllowedSender     Key = "setup_allowed_sender"
	SetupAppID             Key = "setup_app_id"
	SetupAppSecret         Key = "setup_app_secret"
	SetupEnterNumber       Key = "setup_enter_number"
	SetupSelected          Key = "setup_selected"
	SetupArrowHint         Key = "setup_arrow_hint"
	SetupOpenLink          Key = "setup_open_link"
	SetupLinkTTL           Key = "setup_link_ttl"
	SetupReuseAction       Key = "setup_reuse_action"
	SetupReuseKeep         Key = "setup_reuse_keep"
	SetupReuseCreds        Key = "setup_reuse_creds"
	SetupReuseAccess       Key = "setup_reuse_access"
	SetupUsingCached       Key = "setup_using_cached"
	SetupAppSecretKeep     Key = "setup_app_secret_keep"
	CardTitle              Key = "card_title"
	CardStatus             Key = "card_status"
	CardTasks              Key = "card_tasks"
	TasksEmpty             Key = "tasks_empty"
	CardRunning            Key = "card_running"
	CardCompleted          Key = "card_completed"
	CardFailed             Key = "card_failed"
	CardCancelled          Key = "card_cancelled"
	CardEarlierTools       Key = "card_earlier_tools"
	CardExecution          Key = "card_execution"
	CardPlan               Key = "card_plan"
	CardEarlierSteps       Key = "card_earlier_steps"
	CardQuestionTitle      Key = "card_question_title"
	CardQuestionHint       Key = "card_question_hint"
	ModelCurrent           Key = "model_current"
	ModelUnsupported       Key = "model_unsupported"
	ModelUnknown           Key = "model_unknown"
	ModelSwitched          Key = "model_switched"
	ModelAmbiguous         Key = "model_ambiguous"
	HistoryEmpty           Key = "history_empty"
	HistoryList            Key = "history_list"
	HistoryRestored        Key = "history_restored"
	HistoryUnknown         Key = "history_unknown"
	CardRecover            Key = "card_recover"
	TopicNeedsTask         Key = "topic_needs_task"
	TopicAlready           Key = "topic_already"
	TopicFailed            Key = "topic_failed"
	CardInput              Key = "card_input"
	CardOutput             Key = "card_output"
	CardContext            Key = "card_context"
	CardIn                 Key = "card_in"
	CardOut                Key = "card_out"
	CardHit                Key = "card_hit"
	CardWrite              Key = "card_write"
	CardAwaiting           Key = "card_awaiting"
	CardWaking             Key = "card_waking"
	CardStop               Key = "card_stop"
	CardRetry              Key = "card_retry"
	TurnStopRequested      Key = "turn_stop_requested"
	TurnRetryStarted       Key = "turn_retry_started"
	TurnActionExpired      Key = "turn_action_expired"
	CardApprovalTitle      Key = "card_approval_title"
	CardApprovalTool       Key = "card_approval_tool"
	CardApprovalReason     Key = "card_approval_reason"
	CardApprovalRule       Key = "card_approval_rule"
	CardAllowOnce          Key = "card_allow_once"
	CardDeny               Key = "card_deny"
	ApprovalAllowed        Key = "approval_allowed"
	ApprovalRejected       Key = "approval_rejected"
	ApprovalExpired        Key = "approval_expired"
	ApprovalDenied         Key = "approval_denied"
	ApprovalMalformed      Key = "approval_malformed"
	QuotedMessage          Key = "quoted_message"
	ImagePlaceholder       Key = "image_placeholder"
	ImageDownloadFailed    Key = "image_download_failed"
)

type Catalog struct {
	locale Locale
}

func (c Catalog) IsZero() bool { return c.locale == "" }

func New(locale Locale) Catalog {
	if locale != LocaleEN {
		locale = LocaleZH
	}
	return Catalog{locale: locale}
}

func (c Catalog) Locale() Locale {
	if c.locale == "" {
		return LocaleZH
	}
	return c.locale
}

func (c Catalog) T(key Key, args ...any) string {
	table := zh
	if c.Locale() == LocaleEN {
		table = en
	}
	tmpl, ok := table[key]
	if !ok {
		tmpl = string(key)
	}
	if len(args) == 0 {
		return tmpl
	}
	return fmt.Sprintf(tmpl, args...)
}
