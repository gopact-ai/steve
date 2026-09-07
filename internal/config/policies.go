package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/planner"
)

type ExecutionPolicy struct {
	StepTimeout   Duration `json:"step_timeout"`
	VerifyTimeout Duration `json:"verify_timeout"`
}
type PlanningPolicy struct {
	Timeout  Duration `json:"timeout"`
	Attempts int      `json:"attempts"`
}
type SnapshotPolicy struct {
	MaxFiles     int64 `json:"max_files"`
	MaxBytes     int64 `json:"max_bytes"`
	MaxFileBytes int64 `json:"max_file_bytes"`
}
type ReviewPolicy struct {
	MaxChanges   int      `json:"max_changes"`
	MaxDiffBytes int      `json:"max_diff_bytes"`
	MaxFileBytes int      `json:"max_file_bytes"`
	MaxEntries   int      `json:"max_entries"`
	Timeout      Duration `json:"timeout"`
}
type Policies struct {
	Execution ExecutionPolicy `json:"execution"`
	Planning  PlanningPolicy  `json:"planning"`
	Snapshot  SnapshotPolicy  `json:"snapshot"`
	Review    ReviewPolicy    `json:"review"`
}

// Defaults belong to their consumer's policy, not a shared constants bucket.
func (p Policies) WithDefaults() Policies {
	if p.Execution.StepTimeout == 0 {
		p.Execution.StepTimeout = Duration(exec.DefaultStepTimeout)
	}
	if p.Execution.VerifyTimeout == 0 {
		p.Execution.VerifyTimeout = Duration(exec.DefaultVerifyTimeout)
	}
	if p.Planning.Timeout == 0 {
		p.Planning.Timeout = Duration(planner.DefaultTimeout)
	}
	if p.Planning.Attempts == 0 {
		p.Planning.Attempts = planner.DefaultAttempts
	}
	if p.Snapshot.MaxFiles == 0 {
		p.Snapshot.MaxFiles = artifact.MaxSnapshotFiles
	}
	if p.Snapshot.MaxBytes == 0 {
		p.Snapshot.MaxBytes = artifact.MaxSnapshotBytes
	}
	if p.Snapshot.MaxFileBytes == 0 {
		p.Snapshot.MaxFileBytes = artifact.MaxSnapshotFileBytes
	}
	if p.Review.MaxChanges == 0 {
		p.Review.MaxChanges = artifact.MaxChanges
	}
	if p.Review.MaxDiffBytes == 0 {
		p.Review.MaxDiffBytes = artifact.MaxDiffBytes
	}
	if p.Review.MaxFileBytes == 0 {
		p.Review.MaxFileBytes = artifact.MaxFileBytes
	}
	if p.Review.MaxEntries == 0 {
		p.Review.MaxEntries = artifact.MaxEntries
	}
	if p.Review.Timeout == 0 {
		p.Review.Timeout = Duration(artifact.DefaultReviewTimeout)
	}
	return p
}

func (p Policies) Validate() error {
	for name, d := range map[string]Duration{"execution.step_timeout": p.Execution.StepTimeout, "execution.verify_timeout": p.Execution.VerifyTimeout, "planning.timeout": p.Planning.Timeout, "review.timeout": p.Review.Timeout} {
		if d <= 0 {
			return fmt.Errorf("policies.%s must be a positive duration", name)
		}
	}
	for name, n := range map[string]int64{"planning.attempts": int64(p.Planning.Attempts), "snapshot.max_files": p.Snapshot.MaxFiles, "snapshot.max_bytes": p.Snapshot.MaxBytes, "snapshot.max_file_bytes": p.Snapshot.MaxFileBytes, "review.max_changes": int64(p.Review.MaxChanges), "review.max_diff_bytes": int64(p.Review.MaxDiffBytes), "review.max_file_bytes": int64(p.Review.MaxFileBytes), "review.max_entries": int64(p.Review.MaxEntries)} {
		if n < 1 || n > 9_007_199_254_740_991 {
			return fmt.Errorf("policies.%s must be a positive JSON-safe integer", name)
		}
	}
	if p.Snapshot.MaxFileBytes > p.Snapshot.MaxBytes {
		return fmt.Errorf("policies.snapshot.max_file_bytes must not exceed max_bytes")
	}
	return nil
}

// SettingsValues intentionally excludes transport credentials and process env.
type SettingsValues struct {
	Gateway  GatewayPolicy `json:"gateway"`
	Policies Policies      `json:"policies"`
}
type GatewayPolicy struct {
	Locale         string   `json:"locale"`
	OwnerID        string   `json:"owner_id"`
	TaskMaxTurns   int      `json:"task_max_turns"`
	TaskMaxElapsed Duration `json:"task_max_elapsed"`
	PromptTimeout  Duration `json:"prompt_timeout"`
}

func (c *Config) SettingsValues() SettingsValues {
	idle := c.Gateway.PromptTimeout
	if idle <= 0 {
		idle = Duration(10 * time.Minute)
	}
	return SettingsValues{Gateway: GatewayPolicy{Locale: c.Gateway.Locale, OwnerID: c.Gateway.OwnerID, TaskMaxTurns: c.Gateway.TaskMaxTurns, TaskMaxElapsed: c.Gateway.TaskMaxElapsed, PromptTimeout: idle}, Policies: c.Policies.WithDefaults()}
}
func (c *Config) EffectiveLocale() string {
	if c.Gateway.Locale != "" {
		return c.Gateway.Locale
	}
	if c.Feishu.Domain == DomainLark {
		return "en"
	}
	return "zh"
}
func (c *Config) EffectiveOwnerID() string {
	if c.Gateway.OwnerID != "" {
		return c.Gateway.OwnerID
	}
	return c.Feishu.OwnerOpenID
}

func (v SettingsValues) Validate() error {
	if v.Gateway.Locale != "" && v.Gateway.Locale != "zh" && v.Gateway.Locale != "en" {
		return fmt.Errorf("gateway.locale must be zh or en")
	}
	if v.Gateway.TaskMaxTurns < 0 {
		return fmt.Errorf("gateway.task_max_turns must be zero (unlimited) or positive")
	}
	if v.Gateway.TaskMaxElapsed < 0 {
		return fmt.Errorf("gateway.task_max_elapsed must be zero (unlimited) or positive")
	}
	if v.Gateway.PromptTimeout <= 0 {
		return fmt.Errorf("gateway.prompt_timeout must be positive")
	}
	return v.Policies.Validate()
}

func (c *Config) PatchSettings(raw json.RawMessage) (*Config, error) {
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 || trim[0] != '{' {
		return nil, fmt.Errorf("settings must be an object")
	}
	v := c.SettingsValues()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&v); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("settings must contain one object")
	}
	if err := v.Validate(); err != nil {
		return nil, err
	}
	next := *c
	next.Gateway.Locale, next.Gateway.OwnerID = v.Gateway.Locale, strings.TrimSpace(v.Gateway.OwnerID)
	next.Gateway.TaskMaxTurns, next.Gateway.TaskMaxElapsed, next.Gateway.PromptTimeout = v.Gateway.TaskMaxTurns, v.Gateway.TaskMaxElapsed, v.Gateway.PromptTimeout
	next.Policies = v.Policies
	return &next, nil
}
