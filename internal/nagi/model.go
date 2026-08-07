package nagi

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrAlreadyClaimed  = errors.New("task already claimed")
	ErrNotFound        = errors.New("not found")
	ErrNotReady        = errors.New("task is not ready")
	ErrUnsafePath      = errors.New("path is outside the allowed root")
	ErrPrerequisite    = errors.New("host prerequisite unavailable")
	ErrCleanupBlocked  = errors.New("cleanup safety conditions are not satisfied")
	ErrUndraftBlocked  = errors.New("pull request cannot be marked ready")
	ErrInjectedFault   = errors.New("injected fault")
	ErrInvalidArgument = errors.New("invalid argument")
	ErrInvalidState    = errors.New("invalid state transition")
)

type invalidArgumentError struct {
	message string
}

func (err invalidArgumentError) Error() string { return err.message }
func (err invalidArgumentError) Unwrap() error { return ErrInvalidArgument }

func invalidArgument(message string) error {
	return invalidArgumentError{message: message}
}

type Project struct {
	ID         string    `json:"projectId"`
	Repository string    `json:"repository"`
	StateDir   string    `json:"stateDir"`
	DBPath     string    `json:"dbPath"`
	ConfigPath string    `json:"configPath"`
	CreatedAt  time.Time `json:"createdAt"`
}

type Task struct {
	ID              string    `json:"id"`
	ProjectID       string    `json:"projectId"`
	Title           string    `json:"title"`
	ParentID        string    `json:"parentId,omitempty"`
	DependencyID    string    `json:"dependencyId,omitempty"`
	IntegrationLane string    `json:"integrationLane"`
	BaseRef         string    `json:"baseRef"`
	State           string    `json:"state"`
	CreatedAt       time.Time `json:"createdAt"`
}

type Run struct {
	ID               string    `json:"id"`
	ProjectID        string    `json:"projectId"`
	TaskID           string    `json:"taskId"`
	State            string    `json:"state"`
	BaseSHA          string    `json:"baseSha"`
	WorktreePath     string    `json:"worktreePath"`
	Branch           string    `json:"branch"`
	DerivedDataPath  string    `json:"derivedDataPath"`
	RunnerSession    string    `json:"runnerSession"`
	RunnerPID        int       `json:"runnerPid,omitempty"`
	RunnerStatusPath string    `json:"runnerStatusPath"`
	RunnerLogPath    string    `json:"runnerLogPath"`
	FinalSHA         string    `json:"finalSha,omitempty"`
	Disposition      string    `json:"disposition,omitempty"`
	ArtifactsSaved   bool      `json:"artifactsSaved"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

type Operation struct {
	ID            string          `json:"id"`
	RunID         string          `json:"runId"`
	Kind          string          `json:"kind"`
	DesiredState  string          `json:"desiredState"`
	ObservedState string          `json:"observedState"`
	Cursor        string          `json:"cursor,omitempty"`
	Details       json.RawMessage `json:"details,omitempty"`
	CreatedAt     time.Time       `json:"createdAt"`
	UpdatedAt     time.Time       `json:"updatedAt"`
}

type OperationStep struct {
	OperationID string          `json:"operationId"`
	Name        string          `json:"name"`
	State       string          `json:"state"`
	Before      json.RawMessage `json:"before,omitempty"`
	After       json.RawMessage `json:"after,omitempty"`
	UpdatedAt   time.Time       `json:"updatedAt"`
}

type AuditEvent struct {
	ID          int64           `json:"id"`
	ProjectID   string          `json:"projectId"`
	Actor       string          `json:"actor"`
	OperationID string          `json:"operationId,omitempty"`
	Type        string          `json:"type"`
	OccurredAt  time.Time       `json:"occurredAt"`
	Before      json.RawMessage `json:"before,omitempty"`
	After       json.RawMessage `json:"after,omitempty"`
}

type CommandResult struct {
	Argv       []string `json:"argv"`
	CWD        string   `json:"cwd"`
	ExitStatus int      `json:"exitStatus"`
	Stdout     string   `json:"stdout,omitempty"`
	Stderr     string   `json:"stderr,omitempty"`
}

type HostStatus struct {
	Component string   `json:"component"`
	Available bool     `json:"available"`
	Reason    string   `json:"reason"`
	Selected  string   `json:"selected,omitempty"`
	Version   string   `json:"version,omitempty"`
	Argv      []string `json:"argv,omitempty"`
}

type RunnerConfig struct {
	Argv      []string `json:"argv"`
	SeedFiles []string `json:"seedFiles,omitempty"`
}

type ReconcileFinding struct {
	Kind         string `json:"kind"`
	RunID        string `json:"runId,omitempty"`
	Path         string `json:"path"`
	ExpectedSHA  string `json:"expectedSha,omitempty"`
	ObservedSHA  string `json:"observedSha,omitempty"`
	ExpectedRef  string `json:"expectedRef,omitempty"`
	ObservedRef  string `json:"observedRef,omitempty"`
	Dirty        bool   `json:"dirty,omitempty"`
	RequiresUser bool   `json:"requiresUserAction"`
}

type Snapshot struct {
	Project    Project            `json:"project"`
	Tasks      []Task             `json:"tasks"`
	Runs       []Run              `json:"runs"`
	Operations []Operation        `json:"operations"`
	Findings   []ReconcileFinding `json:"findings,omitempty"`
}

type QACriterionSpec struct {
	Name      string   `json:"name"`
	Fixture   string   `json:"fixture,omitempty"`
	Argv      []string `json:"argv"`
	Artifacts []string `json:"artifacts,omitempty"`
}

type QAPacket struct {
	CandidateSHA        string            `json:"candidateSha"`
	Criteria            []QACriterionSpec `json:"criteria"`
	Xcode               string            `json:"xcode"`
	SelectedXcode       string            `json:"selectedXcode,omitempty"`
	ArtifactDestination string            `json:"artifactDestination,omitempty"`
}

type QARun struct {
	ID              string          `json:"id"`
	RunID           string          `json:"runId"`
	CandidateSHA    string          `json:"candidateSha"`
	State           string          `json:"state"`
	WorktreePath    string          `json:"worktreePath"`
	DerivedDataPath string          `json:"derivedDataPath"`
	SelectedXcode   string          `json:"selectedXcode,omitempty"`
	Packet          json.RawMessage `json:"packet"`
	ArtifactDir     string          `json:"artifactDir"`
	ValidatedSHA    string          `json:"validatedSha,omitempty"`
	CreatedAt       time.Time       `json:"createdAt"`
	UpdatedAt       time.Time       `json:"updatedAt"`
}

type QACriterionResult struct {
	QAID            string          `json:"qaId"`
	Name            string          `json:"name"`
	Status          string          `json:"status"`
	Observed        json.RawMessage `json:"observed"`
	Reproducibility string          `json:"reproducibility"`
	Artifacts       []string        `json:"artifacts"`
}

type PullRequest struct {
	RunID         string    `json:"runId"`
	Number        int       `json:"number"`
	NodeID        string    `json:"nodeId,omitempty"`
	TargetBranch  string    `json:"targetBranch"`
	HeadBranch    string    `json:"headBranch"`
	ExpectedState string    `json:"expectedState"`
	ObservedState string    `json:"observedState"`
	HeadSHA       string    `json:"headSha"`
	CIState       string    `json:"ciState"`
	ConflictState string    `json:"conflictState"`
	ReviewState   string    `json:"reviewState"`
	Cursor        string    `json:"cursor"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type PRObservation struct {
	Number        int    `json:"number"`
	NodeID        string `json:"nodeId"`
	Draft         bool   `json:"draft"`
	TargetBranch  string `json:"targetBranch"`
	HeadBranch    string `json:"headBranch"`
	HeadSHA       string `json:"headSha"`
	CIState       string `json:"ciState"`
	ConflictState string `json:"conflictState"`
	ReviewState   string `json:"reviewState"`
}

type UndraftDecision struct {
	Ready   bool        `json:"ready"`
	Reasons []string    `json:"reasons"`
	PR      PullRequest `json:"pullRequest"`
}
