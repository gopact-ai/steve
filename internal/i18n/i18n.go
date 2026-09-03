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
	CardProject            Key = "card_project"
	ProjectCurrent         Key = "project_current"
	ProjectListHeader      Key = "project_list_header"
	ProjectSwitched        Key = "project_switched"
	ProjectUnknown         Key = "project_unknown"
	ProjectUsage           Key = "project_usage"
	ProjectNotHome         Key = "project_not_home"
	ProjectUnbound         Key = "project_unbound"
	ProjectsDisabled       Key = "projects_disabled"
	ProjectBusy            Key = "project_busy"
	ProjectLevel           Key = "project_level"
	ProjectAccess          Key = "project_access"
	OwnerOnly              Key = "owner_only"
	GrantUsage             Key = "grant_usage"
	GrantDone              Key = "grant_done"
	GrantHeader            Key = "grant_header"
	CardDisclosure         Key = "card_disclosure"
	DisclosurePending      Key = "disclosure_pending"
	DisclosureApproved     Key = "disclosure_approved"
	DisclosureDenied       Key = "disclosure_denied"
	DisclosureUnknown      Key = "disclosure_unknown"
	DisclosureUsage        Key = "disclosure_usage"
	CardEffects            Key = "card_effects"
	EffectsNone            Key = "effects_none"
	EffectsHeader          Key = "effects_header"
	EffectsResolved        Key = "effects_resolved"
	EffectsUsage           Key = "effects_usage"
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
	SetupOwnerHint         Key = "setup_owner_hint"
	SetupOwnerPrompt       Key = "setup_owner_prompt"
	SetupOwnerBound        Key = "setup_owner_bound"
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
	TasksUsage             Key = "tasks_usage"
	TaskUnknown            Key = "task_unknown"
	TaskNone               Key = "task_none"
	TaskNonePaused         Key = "task_none_paused"
	TaskPaused             Key = "task_paused"
	TaskCancelled          Key = "task_cancelled"
	TaskResumed            Key = "task_resumed"
	TaskResumeNotice       Key = "task_resume_notice"
	TaskResumeManual       Key = "task_resume_manual"
	TaskStuck              Key = "task_stuck"
	TaskAttempts           Key = "task_attempts"
	TaskWhereItGot         Key = "task_where_it_got"
	TaskOfflineDone        Key = "task_offline_done"
	TaskDropped            Key = "task_dropped"
	CardSchedules          Key = "card_schedules"
	CardFleet              Key = "card_fleet"
	PlanDisabled           Key = "plan_disabled"
	PlanUsage              Key = "plan_usage"
	PlanFailed             Key = "plan_failed"
	PlanStopped            Key = "plan_stopped"
	PlanDone               Key = "plan_done"
	PlanRecovered          Key = "plan_recovered"
	PlanRevisions          Key = "plan_revisions"
	PlanUnknown            Key = "plan_unknown"
	PlansEmpty             Key = "plans_empty"
	FleetLocal             Key = "fleet_local"
	FleetRepairHint        Key = "fleet_repair_hint"
	FleetProbeNothing      Key = "fleet_probe_nothing"
	FleetProbeSilent       Key = "fleet_probe_silent"
	CardRepair             Key = "card_repair"
	RepairUsage            Key = "repair_usage"
	RepairDisabled         Key = "repair_disabled"
	RepairNotNeeded        Key = "repair_not_needed"
	RepairImpossible       Key = "repair_impossible"
	RepairGoal             Key = "repair_goal"
	RepairDone             Key = "repair_done"
	RepairStopped          Key = "repair_stopped"
	RepairStillBroken      Key = "repair_still_broken"
	SchedulesEmpty         Key = "schedules_empty"
	ScheduleUsage          Key = "schedule_usage"
	ScheduleBad            Key = "schedule_bad"
	ScheduleCreated        Key = "schedule_created"
	ScheduleRefused        Key = "schedule_refused"
	ScheduleCancelled      Key = "schedule_cancelled"
	ScheduleUnknown        Key = "schedule_unknown"
	ScheduleNotice         Key = "schedule_notice"
	ScheduleEveryLabel     Key = "schedule_every_label"
	ScheduleDailyLabel     Key = "schedule_daily_label"
	ScheduleOnceLabel      Key = "schedule_once_label"
	ScheduleNextLabel      Key = "schedule_next_label"
	ScheduleRunsLabel      Key = "schedule_runs_label"
	ResumeNotice           Key = "resume_notice"
	ResumePrompt           Key = "resume_prompt"
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
	CardSentTo             Key = "card_sent_to"
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
