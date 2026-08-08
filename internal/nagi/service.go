package nagi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Service struct {
	Store   *Store
	Project Project
	Clock   Clock
	IDs     IDSource
	Git     GitAdapter
	Runner  ProcessRunner
	Exec    CommandExecutor
	GitHub  GitHub
}

type StartOptions struct {
	Actor      string
	FaultAfter string
}

type CleanupOptions struct {
	Actor      string
	FaultAfter string
}

type registryProject struct {
	ID         string `json:"projectId"`
	Repository string `json:"repository"`
	StateDir   string `json:"stateDir"`
	DBPath     string `json:"dbPath"`
	ConfigPath string `json:"configPath"`
	CreatedAt  string `json:"createdAt"`
}

func DefaultStateRoot() (string, error) {
	if configured := os.Getenv("NAGI_STATE_ROOT"); configured != "" {
		return filepath.Abs(configured)
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "nagi"), nil
}

func Initialize(ctx context.Context, repository, stateRoot, configPath string, clock Clock, ids IDSource) (Project, map[string]any, error) {
	if clock == nil {
		clock = SystemClock{}
	}
	if ids == nil {
		ids = RandomIDs{}
	}
	if stateRoot == "" {
		var err error
		stateRoot, err = DefaultStateRoot()
		if err != nil {
			return Project{}, nil, err
		}
	}
	stateRoot, err := filepath.Abs(stateRoot)
	if err != nil {
		return Project{}, nil, err
	}
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		return Project{}, nil, err
	}
	if resolvedStateRoot, resolveErr := filepath.EvalSymlinks(stateRoot); resolveErr == nil {
		stateRoot = resolvedStateRoot
	}
	if configPath == "" {
		configPath = ".nagi.json"
	}
	if err := validateRelative(configPath); err != nil {
		return Project{}, nil, fmt.Errorf("config path: %w", err)
	}
	repository, err = filepath.Abs(repository)
	if err != nil {
		return Project{}, nil, err
	}
	repository, err = filepath.EvalSymlinks(repository)
	if err != nil {
		return Project{}, nil, err
	}
	git := GitAdapter{}
	if _, err := git.RevParse(ctx, repository, "HEAD"); err != nil {
		return Project{}, nil, fmt.Errorf("repository: %w", err)
	}
	projectID := ids.NewID("project")
	projectDir := filepath.Join(stateRoot, "projects", projectID)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		return Project{}, nil, err
	}
	project := Project{
		ID: projectID, Repository: repository, StateDir: projectDir,
		DBPath: filepath.Join(projectDir, "nagi.sqlite"), ConfigPath: configPath,
		CreatedAt: clock.Now(),
	}
	store, err := OpenStore(project.DBPath, clock)
	if err != nil {
		return Project{}, nil, err
	}
	defer store.Close()
	if err := store.CreateProject(ctx, project, "init", ids.NewID("operation")); err != nil {
		return Project{}, nil, err
	}
	metadata := registryProject{ID: project.ID, Repository: project.Repository, StateDir: project.StateDir, DBPath: project.DBPath, ConfigPath: project.ConfigPath, CreatedAt: formatTime(project.CreatedAt)}
	content, _ := json.MarshalIndent(metadata, "", "  ")
	if err := writeAtomic(filepath.Join(projectDir, "project.json"), content, 0o600); err != nil {
		return Project{}, nil, err
	}
	verification, err := store.Verify(ctx)
	if err != nil {
		return Project{}, nil, err
	}
	return project, verification, nil
}

func OpenService(ctx context.Context, projectID, stateRoot string, clock Clock, ids IDSource) (*Service, error) {
	if stateRoot == "" {
		var err error
		stateRoot, err = DefaultStateRoot()
		if err != nil {
			return nil, err
		}
	}
	metadataPath := filepath.Join(stateRoot, "projects", projectID, "project.json")
	content, err := os.ReadFile(metadataPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var metadata registryProject
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return nil, err
	}
	if metadata.ID != projectID {
		return nil, fmt.Errorf("project metadata ID mismatch")
	}
	if clock == nil {
		clock = SystemClock{}
	}
	if ids == nil {
		ids = RandomIDs{}
	}
	store, err := OpenStore(metadata.DBPath, clock)
	if err != nil {
		return nil, err
	}
	project, err := store.Project(ctx, projectID)
	if err != nil {
		store.Close()
		return nil, err
	}
	executor := OSExecutor{}
	processRunner := OSProcessRunner{IDs: ids}
	service := &Service{Store: store, Project: project, Clock: clock, IDs: ids, Exec: DurableExecutor{Direct: executor, Runner: processRunner}}
	service.Git = GitAdapter{Exec: executor}
	service.Runner = processRunner
	service.GitHub = GHAdapter{Exec: executor}
	return service, nil
}

func (s *Service) Close() error { return s.Store.Close() }

func (s *Service) AddTask(ctx context.Context, task Task, actor string) (Task, error) {
	if task.ID == "" || task.Title == "" {
		return Task{}, invalidArgument("task id and title are required")
	}
	if task.IntegrationLane != "base" && task.IntegrationLane != "master" {
		return Task{}, invalidArgument("integration lane must be base or master")
	}
	if task.BaseRef == "" {
		task.BaseRef = "master"
	}
	task.ProjectID, task.State, task.CreatedAt = s.Project.ID, "ready", s.Clock.Now()
	if task.ParentID != "" {
		if _, err := s.Store.Task(ctx, task.ParentID); err != nil {
			return Task{}, fmt.Errorf("parent task: %w", err)
		}
	}
	if task.DependencyID != "" {
		if _, err := s.Store.Task(ctx, task.DependencyID); err != nil {
			return Task{}, fmt.Errorf("dependency task: %w", err)
		}
	}
	if err := s.Store.AddTask(ctx, task, actor, s.IDs.NewID("operation")); err != nil {
		return Task{}, err
	}
	return task, nil
}

func (s *Service) RegisterSeed(source, name string) (string, error) {
	if err := validateRelative(name); err != nil {
		return "", err
	}
	info, err := os.Stat(source)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("seed is not a regular file")
	}
	destination, err := containedPath(filepath.Join(s.Project.StateDir, "seeds"), name, false)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return "", err
	}
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return destination, nil
}

func (s *Service) StartTask(ctx context.Context, taskID string, options StartOptions) (Run, error) {
	task, err := s.Store.Task(ctx, taskID)
	if err != nil {
		return Run{}, err
	}
	baseRef := task.BaseRef
	if task.DependencyID != "" {
		dependency, err := s.Store.RunForTask(ctx, task.DependencyID)
		if err != nil || dependency.State != "completed" || dependency.FinalSHA == "" {
			return Run{}, ErrNotReady
		}
		if task.IntegrationLane == "base" {
			baseRef = dependency.FinalSHA
		}
	}
	baseSHA, err := s.Git.RevParse(ctx, s.Project.Repository, baseRef)
	if err != nil {
		return Run{}, err
	}
	runID, operationID := s.IDs.NewID("run"), s.IDs.NewID("operation")
	worktree := filepath.Join(s.Project.StateDir, "worktrees", runID)
	run := Run{
		ID: runID, ProjectID: s.Project.ID, TaskID: task.ID, State: "provisioning", BaseSHA: baseSHA,
		WorktreePath: worktree, Branch: "nagi/" + task.ID + "/" + runID,
		DerivedDataPath:  filepath.Join(s.Project.StateDir, "derived-data", runID),
		RunnerSession:    "runner/" + operationID,
		RunnerStatusPath: filepath.Join(s.Project.StateDir, "runner", runID+".status.json"),
		RunnerLogPath:    filepath.Join(s.Project.StateDir, "artifacts", runID, "runner.log"),
		CreatedAt:        s.Clock.Now(), UpdatedAt: s.Clock.Now(),
	}
	details, _ := json.Marshal(map[string]any{"worktreePath": run.WorktreePath, "branch": run.Branch, "baseSha": run.BaseSHA, "derivedDataPath": run.DerivedDataPath, "runnerSession": run.RunnerSession})
	operation := Operation{ID: operationID, RunID: run.ID, Kind: "provision", DesiredState: "running", ObservedState: "planned", Details: details, CreatedAt: s.Clock.Now(), UpdatedAt: s.Clock.Now()}
	if _, err := s.Store.ClaimTask(ctx, task, run, operation, options.Actor); err != nil {
		return Run{}, fmt.Errorf("claim task: %w", err)
	}
	if options.FaultAfter == "plan" {
		return run, ErrInjectedFault
	}
	return s.provision(ctx, run, operation, options)
}

func (s *Service) ResumeProvisioning(ctx context.Context, actor string) ([]Run, error) {
	operations, err := s.Store.Operations(ctx, s.Project.ID)
	if err != nil {
		return nil, err
	}
	var resumed []Run
	for _, operation := range operations {
		if operation.Kind != "provision" || operation.ObservedState == operation.DesiredState {
			continue
		}
		run, err := s.Store.Run(ctx, operation.RunID)
		if err != nil {
			return resumed, err
		}
		run, err = s.provision(ctx, run, operation, StartOptions{Actor: actor})
		if err != nil {
			return resumed, err
		}
		resumed = append(resumed, run)
	}
	return resumed, nil
}

func (s *Service) provision(ctx context.Context, run Run, operation Operation, options StartOptions) (Run, error) {
	if err := s.Git.AddWorktree(ctx, s.Project.Repository, run); err != nil {
		return run, err
	}
	if err := s.Store.CompleteStep(ctx, s.Project.ID, options.Actor, operation, "worktree", nil, map[string]any{"path": run.WorktreePath, "sha": run.BaseSHA, "branch": run.Branch}); err != nil {
		return run, fmt.Errorf("record worktree step: %w", err)
	}
	if options.FaultAfter == "worktree" {
		return run, ErrInjectedFault
	}
	configContent, err := s.Git.ShowFile(ctx, s.Project.Repository, run.BaseSHA, s.Project.ConfigPath)
	if err != nil {
		return run, err
	}
	config, err := LoadRunnerConfig(configContent)
	if err != nil {
		return run, err
	}
	if err := CopySeedFiles(filepath.Join(s.Project.StateDir, "seeds"), run.WorktreePath, config.SeedFiles); err != nil && len(config.SeedFiles) > 0 {
		return run, err
	}
	if err := os.MkdirAll(run.DerivedDataPath, 0o700); err != nil {
		return run, err
	}
	if err := s.Store.CompleteStep(ctx, s.Project.ID, options.Actor, operation, "isolated_paths", nil, map[string]any{"derivedDataPath": run.DerivedDataPath, "seedCount": len(config.SeedFiles)}); err != nil {
		return run, fmt.Errorf("record isolated paths step: %w", err)
	}
	if options.FaultAfter == "paths" {
		return run, ErrInjectedFault
	}
	process, err := s.Runner.Start(ctx, RunnerStart{Run: run, Argv: config.Argv, Environment: runnerEnvironment(run)})
	if err != nil {
		return run, err
	}
	if err := s.Store.CompleteStep(ctx, s.Project.ID, options.Actor, operation, "runner", nil, process); err != nil {
		return run, fmt.Errorf("record runner step: %w", err)
	}
	if options.FaultAfter == "runner" {
		return run, ErrInjectedFault
	}
	run, err = s.Store.UpdateRunProvisioned(ctx, run, process, options.Actor, operation.ID)
	if err != nil {
		return run, fmt.Errorf("record running state: %w", err)
	}
	details, _ := json.Marshal(map[string]any{"pid": process.PID, "sessionId": process.SessionID, "worktreePath": run.WorktreePath})
	_, err = s.Store.UpdateOperation(ctx, s.Project.ID, options.Actor, operation, "running", operation.Cursor, details)
	if err != nil {
		return run, fmt.Errorf("record provision operation: %w", err)
	}
	return run, nil
}

func (s *Service) Snapshot(ctx context.Context) (Snapshot, error) {
	tasks, err := s.Store.Tasks(ctx, s.Project.ID)
	if err != nil {
		return Snapshot{}, err
	}
	runs, err := s.Store.Runs(ctx, s.Project.ID)
	if err != nil {
		return Snapshot{}, err
	}
	operations, err := s.Store.Operations(ctx, s.Project.ID)
	if err != nil {
		return Snapshot{}, err
	}
	findings, err := s.ReconcileWorktrees(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Project: s.Project, Tasks: tasks, Runs: runs, Operations: operations, Findings: findings}, nil
}

func (s *Service) ReconcileWorktrees(ctx context.Context) ([]ReconcileFinding, error) {
	runs, err := s.Store.Runs(ctx, s.Project.ID)
	if err != nil {
		return nil, err
	}
	actual, err := s.Git.ListWorktrees(ctx, s.Project.Repository)
	if err != nil {
		return nil, err
	}
	expectedByPath := make(map[string]Run, len(runs))
	actualByPath := make(map[string]GitWorktree, len(actual))
	for _, run := range runs {
		expectedByPath[filepath.Clean(run.WorktreePath)] = run
	}
	for _, worktree := range actual {
		actualByPath[filepath.Clean(worktree.Path)] = worktree
	}
	var findings []ReconcileFinding
	for path, run := range expectedByPath {
		observed, ok := actualByPath[path]
		expectedSHA := run.BaseSHA
		if run.FinalSHA != "" {
			expectedSHA = run.FinalSHA
		}
		if !ok {
			findings = append(findings, ReconcileFinding{Kind: "lost_worktree", RunID: run.ID, Path: path, ExpectedSHA: expectedSHA, ExpectedRef: "refs/heads/" + run.Branch, RequiresUser: true})
			continue
		}
		if observed.HEAD != expectedSHA || observed.Branch != "refs/heads/"+run.Branch {
			findings = append(findings, ReconcileFinding{Kind: "state_mismatch", RunID: run.ID, Path: path, ExpectedSHA: expectedSHA, ObservedSHA: observed.HEAD, ExpectedRef: "refs/heads/" + run.Branch, ObservedRef: observed.Branch, RequiresUser: true})
		}
		dirty, dirtyErr := s.Git.Dirty(ctx, path)
		if dirtyErr == nil && dirty {
			findings = append(findings, ReconcileFinding{Kind: "dirty", RunID: run.ID, Path: path, ObservedSHA: observed.HEAD, ObservedRef: observed.Branch, Dirty: true, RequiresUser: true})
		}
	}
	for path, observed := range actualByPath {
		if path == filepath.Clean(s.Project.Repository) {
			continue
		}
		if _, ok := expectedByPath[path]; !ok {
			dirty, _ := s.Git.Dirty(ctx, path)
			findings = append(findings, ReconcileFinding{Kind: "unmanaged", Path: path, ObservedSHA: observed.HEAD, ObservedRef: observed.Branch, Dirty: dirty, RequiresUser: true})
		}
	}
	return findings, nil
}

func (s *Service) CancelRun(ctx context.Context, runID, actor string) (Run, error) {
	run, err := s.Store.Run(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	op := Operation{ID: s.IDs.NewID("operation"), RunID: run.ID, Kind: "cancel", DesiredState: "cancelled", ObservedState: "planned", CreatedAt: s.Clock.Now(), UpdatedAt: s.Clock.Now()}
	op, err = s.Store.EnsureOperation(ctx, op)
	if err != nil {
		return Run{}, err
	}
	if err := s.Runner.Cancel(ctx, run); err != nil {
		return Run{}, err
	}
	if err := s.Store.CompleteStep(ctx, s.Project.ID, actor, op, "runner_cancelled", nil, map[string]any{"pid": run.RunnerPID}); err != nil {
		return Run{}, err
	}
	run, err = s.Store.UpdateRunState(ctx, run, "cancelled", "", "", false, actor, op.ID)
	if err != nil {
		return Run{}, err
	}
	details, _ := json.Marshal(map[string]any{"runId": run.ID, "state": run.State})
	_, err = s.Store.UpdateOperation(ctx, s.Project.ID, actor, op, "cancelled", op.Cursor, details)
	return run, err
}

func (s *Service) CompleteRun(ctx context.Context, runID, disposition, actor string) (Run, error) {
	if disposition != "integrated" && disposition != "discarded" {
		return Run{}, fmt.Errorf("disposition must be integrated or discarded")
	}
	run, err := s.Store.Run(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	op := Operation{ID: s.IDs.NewID("operation"), RunID: run.ID, Kind: "complete/" + disposition, DesiredState: "completed", ObservedState: "planned", CreatedAt: s.Clock.Now(), UpdatedAt: s.Clock.Now()}
	op, err = s.Store.EnsureOperation(ctx, op)
	if err != nil {
		return Run{}, err
	}
	observation, err := s.Runner.Observe(ctx, run)
	if err != nil {
		return Run{}, err
	}
	if observation.State == "running" || observation.State == "not_started" {
		return Run{}, fmt.Errorf("runner is not stopped: %w", ErrInvalidState)
	}
	finalSHA, err := s.Git.RevParse(ctx, run.WorktreePath, "HEAD")
	if err != nil {
		return Run{}, err
	}
	task, err := s.Store.Task(ctx, run.TaskID)
	if err != nil {
		return Run{}, err
	}
	if disposition == "integrated" {
		integrated, err := s.Git.IsAncestor(ctx, s.Project.Repository, finalSHA, task.BaseRef)
		if err != nil {
			return Run{}, err
		}
		if !integrated {
			return Run{}, IntegrationBlockedError{FinalSHA: finalSHA, BaseRef: task.BaseRef}
		}
	}
	_, artifactErr := os.Stat(run.RunnerLogPath)
	artifactsSaved := artifactErr == nil
	run, err = s.Store.UpdateRunState(ctx, run, "completed", finalSHA, disposition, artifactsSaved, actor, op.ID)
	if err != nil {
		return Run{}, err
	}
	details, _ := json.Marshal(map[string]any{"runId": run.ID, "state": run.State, "finalSha": run.FinalSHA, "disposition": run.Disposition, "artifactsSaved": run.ArtifactsSaved})
	_, err = s.Store.UpdateOperation(ctx, s.Project.ID, actor, op, "completed", op.Cursor, details)
	return run, err
}

func (s *Service) CleanupRun(ctx context.Context, runID string, options CleanupOptions) (map[string]any, error) {
	run, err := s.Store.Run(ctx, runID)
	if err != nil {
		return nil, err
	}
	task, err := s.Store.Task(ctx, run.TaskID)
	if err != nil {
		return nil, err
	}
	op := Operation{ID: s.IDs.NewID("operation"), RunID: run.ID, Kind: "cleanup", DesiredState: "cleaned", ObservedState: "planned", CreatedAt: s.Clock.Now(), UpdatedAt: s.Clock.Now()}
	op, err = s.Store.EnsureOperation(ctx, op)
	if err != nil {
		return nil, err
	}
	reasons, err := s.cleanupBlockers(ctx, run, task, op)
	if err != nil {
		return nil, err
	}
	if len(reasons) > 0 {
		return map[string]any{"deleted": false, "reasons": reasons}, ErrCleanupBlocked
	}
	steps := []struct {
		name   string
		action func() (any, error)
	}{
		{"agent_stopped", func() (any, error) { observation, err := s.Runner.Observe(ctx, run); return observation, err }},
		{"final_sha_saved", func() (any, error) { return map[string]string{"finalSha": run.FinalSHA}, nil }},
		{"artifacts_saved", func() (any, error) {
			_, err := os.Stat(run.RunnerLogPath)
			return map[string]string{"runnerLog": run.RunnerLogPath}, err
		}},
		{"worktree_removed", func() (any, error) {
			err := s.Git.RemoveWorktree(ctx, s.Project.Repository, run.WorktreePath)
			return map[string]string{"path": run.WorktreePath}, err
		}},
		{"derived_data_removed", func() (any, error) {
			if err := ensureManagedPath(s.Project.StateDir, run.DerivedDataPath); err != nil {
				return nil, err
			}
			err := os.RemoveAll(run.DerivedDataPath)
			return map[string]string{"path": run.DerivedDataPath}, err
		}},
		{"branch_handled", func() (any, error) {
			if run.Disposition == "discarded" {
				return map[string]string{"branch": run.Branch, "action": "preserved_unmerged"}, nil
			}
			err := s.Git.DeleteMergedBranch(ctx, s.Project.Repository, run.Branch)
			return map[string]string{"branch": run.Branch, "action": "deleted_if_merged"}, err
		}},
	}
	for _, step := range steps {
		if existing, stepErr := s.Store.Step(ctx, op.ID, step.name); stepErr == nil && existing.State == "completed" {
			continue
		}
		after, actionErr := step.action()
		if actionErr != nil {
			return map[string]any{"deleted": false, "failedStep": step.name, "reason": actionErr.Error()}, actionErr
		}
		if err := s.Store.CompleteStep(ctx, s.Project.ID, options.Actor, op, step.name, map[string]any{"runId": run.ID}, after); err != nil {
			return nil, err
		}
		if options.FaultAfter == step.name {
			return map[string]any{"deleted": false, "stoppedAfter": step.name}, ErrInjectedFault
		}
	}
	result := map[string]any{"deleted": true, "worktree": run.WorktreePath, "derivedData": run.DerivedDataPath}
	details, _ := json.Marshal(result)
	_, err = s.Store.UpdateOperation(ctx, s.Project.ID, options.Actor, op, "cleaned", op.Cursor, details)
	return result, err
}

func (s *Service) cleanupBlockers(ctx context.Context, run Run, task Task, op Operation) ([]string, error) {
	var reasons []string
	observation, err := s.Runner.Observe(ctx, run)
	if err != nil {
		return nil, err
	}
	if observation.State == "running" || observation.State == "not_started" {
		reasons = append(reasons, "runner_not_stopped")
	}
	worktreeRemoved := false
	if step, stepErr := s.Store.Step(ctx, op.ID, "worktree_removed"); stepErr == nil && step.State == "completed" {
		worktreeRemoved = true
	}
	if !worktreeRemoved {
		actualSHA, err := s.Git.RevParse(ctx, run.WorktreePath, "HEAD")
		if err != nil {
			reasons = append(reasons, "worktree_missing")
		} else if run.FinalSHA == "" || run.FinalSHA != actualSHA {
			reasons = append(reasons, "final_sha_not_observed")
		}
	}
	if run.Disposition != "integrated" && run.Disposition != "discarded" {
		reasons = append(reasons, "integration_or_discard_not_recorded")
	}
	if run.Disposition == "integrated" && run.FinalSHA != "" {
		integrated, checkErr := s.Git.IsAncestor(ctx, s.Project.Repository, run.FinalSHA, task.BaseRef)
		if checkErr != nil {
			return nil, checkErr
		}
		if !integrated {
			reasons = append(reasons, "final_sha_not_integrated")
		}
	}
	if !worktreeRemoved {
		if dirty, dirtyErr := s.Git.Dirty(ctx, run.WorktreePath); dirtyErr == nil && dirty {
			reasons = append(reasons, "worktree_dirty")
		}
	}
	if _, artifactErr := os.Stat(run.RunnerLogPath); artifactErr != nil || !run.ArtifactsSaved {
		reasons = append(reasons, "artifacts_not_saved")
	}
	return reasons, nil
}

func writeAtomic(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".nagi-project-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func ensureManagedPath(root, target string) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return ErrUnsafePath
	}
	return nil
}
