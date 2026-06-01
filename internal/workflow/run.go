package workflow

import "time"

type Status string

const (
	StatusQueued  Status = "queued"
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
)

type StageStatus string

const (
	StagePending StageStatus = "pending"
	StageRunning StageStatus = "running"
	StageDone    StageStatus = "done"
	StageFailed  StageStatus = "failed"
)

type StageKind string

const (
	KindResearch  StageKind = "research"
	KindPlan      StageKind = "plan"
	KindImplement StageKind = "implement"
	KindReview    StageKind = "review"   // reserved for future use
	KindVerify    StageKind = "verify"   // reserved for future use
	KindMerge     StageKind = "merge"
)

type Stage struct {
	ID              string
	Kind            StageKind
	SessionID       string      // the VM session executing this stage (empty until started)
	Status          StageStatus
	Input           string      // prompt fed to this stage
	Output          string      // captured stdout, available when StageDone
	Error           string      // set when Status == StageFailed; reason for failure
	VerifySessionID string      // session ID of the verify VM for this implement stage
}

// WorkflowConfig holds optional per-workflow overrides. The zero value uses all defaults.
type WorkflowConfig struct {
	VerifierPrompt string // empty = use default premade verifier rubric; special harness fills this in
}

type Workflow struct {
	ID        string
	Input     string    // the user's goal / brief for the entire workflow
	RepoURL   string
	Branch    string
	Status    Status
	Stages    []Stage
	Error     string
	Config    WorkflowConfig
	CreatedAt time.Time
	UpdatedAt time.Time
}
