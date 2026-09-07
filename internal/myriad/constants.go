package myriad

import "time"

const (
	StatusCreated     = "CREATED"
	StatusRunning     = "RUNNING"
	StatusReady       = "READY_TO_INTEGRATE"
	StatusIntegrating = "INTEGRATING"
	StatusValidating  = "VALIDATING"
	StatusIntegrated  = "INTEGRATED"
	StatusCompleted   = "COMPLETED_NO_CHANGES"
	StatusFailed      = "FAILED"
	StatusRecovery    = "RECOVERY_REQUIRED"

	MemoryName       = ".ai-memory"
	TaskRecordSchema = 1
	SessionSchema    = 1
	InboxSchema      = 1
	ContextSchema    = 1

	maxJSONBytes        = 8 * 1024 * 1024
	maxMemoryBytes      = 1024 * 1024
	maxInboxBytes       = 1024 * 1024
	maxHookInputBytes   = 1024 * 1024
	maxCodexRPCBytes    = 32 * 1024 * 1024
	validationTailBytes = 16 * 1024
	validationHistory   = 16
	integrationHistory  = 32
	publishHistory      = 32
	lifecycleHistory    = 64
	defaultCheckTimeout = time.Hour

	handoffExitCode = 75

	internalSupervise    = "__supervise"
	internalValidate     = "__validate"
	internalLease        = "__lease"
	internalInboxHook    = "__inbox-hook"
	internalActivityHook = "__activity-hook"
	internalProvision    = "__provision-hook"

	envInheritedLockFDs   = "MYRIAD_INHERITED_LOCK_FDS"
	envLockSessionPath    = "MYRIAD_LOCK_SESSION_PATH"
	envLockSessionID      = "MYRIAD_LOCK_SESSION_ID"
	envForegroundPGID     = "MYRIAD_FOREGROUND_PGID"
	envAgentSessionPath   = "MYRIAD_SESSION_PATH"
	envAgentSessionID     = "MYRIAD_SESSION_ID"
	envCodexRecoveryCWD   = "MYRIAD_CODEX_RECOVERY_CWD"
	envCodexPrivateScreen = "MYRIAD_CODEX_PRIVATE_SCREEN"

	worktreePending  = "pending"
	worktreeCreating = "creating"
	worktreeReady    = "ready"

	codexSlugModel        = "gpt-5.6-luna"
	codexSlugEffort       = "medium"
	codexSlugLimit        = 48
	codexSlugPreviewLimit = 4000
	codexStatusLine       = `tui.status_line=["thread-title","pull-request-number","current-dir","model-with-reasoning"]`

	notificationProtocol = 1
)

var forbiddenLocalPaths = []string{MemoryName}

var genericTaskDescriptions = map[string]struct{}{
	"interactive agent task":       {},
	"interactive task":             {},
	"resume a saved codex session": {},
}
