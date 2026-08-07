package nagi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"time"
)

type Clock interface {
	Now() time.Time
}

type IDSource interface {
	NewID(prefix string) string
}

type CommandExecutor interface {
	Run(ctx context.Context, cwd string, env []string, argv ...string) (CommandResult, error)
}

type ResumableCommandExecutor interface {
	CommandExecutor
	RunResumable(ctx context.Context, spec ResumableCommand) (CommandResult, error)
}

type ResumableCommand struct {
	ID          string
	CWD         string
	Environment []string
	Argv        []string
	StatusPath  string
	LogPath     string
}

type ProcessRunner interface {
	Start(ctx context.Context, spec RunnerStart) (RunnerProcess, error)
	Observe(ctx context.Context, run Run) (RunnerObservation, error)
	Cancel(ctx context.Context, run Run) error
}

type GitHub interface {
	FindPullRequest(ctx context.Context, repository, headOwner, headBranch, targetBranch string) (*PRObservation, error)
	CreateDraftPullRequest(ctx context.Context, repository, title, headBranch, targetBranch string) (PRObservation, error)
	ObservePullRequest(ctx context.Context, repository string, number int) (PRObservation, error)
	MarkReady(ctx context.Context, repository, nodeID string) error
}

type RunnerStart struct {
	Run         Run
	Argv        []string
	Environment []string
}

type RunnerProcess struct {
	PID        int    `json:"pid"`
	SessionID  string `json:"sessionId"`
	StatusPath string `json:"statusPath"`
	LogPath    string `json:"logPath"`
}

type RunnerObservation struct {
	State      string `json:"state"`
	ExitStatus *int   `json:"exitStatus,omitempty"`
}

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

type RandomIDs struct{}

func (RandomIDs) NewID(prefix string) string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(raw[:])
}

type FixedClock struct{ Time time.Time }

func (c FixedClock) Now() time.Time { return c.Time }

type SequenceIDs struct {
	Values []string
	index  int
}

func (s *SequenceIDs) NewID(prefix string) string {
	if s.index < len(s.Values) {
		value := s.Values[s.index]
		s.index++
		return value
	}
	return RandomIDs{}.NewID(prefix)
}

func runnerEnvironment(run Run) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"NAGI_RUN_ID=" + run.ID,
		"NAGI_WORKTREE=" + run.WorktreePath,
		"NAGI_DERIVED_DATA=" + run.DerivedDataPath,
	}
}
