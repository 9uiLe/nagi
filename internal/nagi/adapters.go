package nagi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

type OSExecutor struct{}

func (OSExecutor) Run(ctx context.Context, cwd string, env []string, argv ...string) (CommandResult, error) {
	result := CommandResult{Argv: append([]string(nil), argv...), CWD: cwd}
	if len(argv) == 0 {
		return result, errors.New("empty argv")
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = cwd
	if env != nil {
		command.Env = append([]string(nil), env...)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result.Stdout, result.Stderr = stdout.String(), stderr.String()
	if err == nil {
		return result, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.ExitStatus = exitError.ExitCode()
		return result, nil
	}
	result.ExitStatus = -1
	return result, err
}

type DurableExecutor struct {
	Direct CommandExecutor
	Runner ProcessRunner
}

func (d DurableExecutor) Run(ctx context.Context, cwd string, env []string, argv ...string) (CommandResult, error) {
	direct := d.Direct
	if direct == nil {
		direct = OSExecutor{}
	}
	return direct.Run(ctx, cwd, env, argv...)
}

func (d DurableExecutor) RunResumable(ctx context.Context, spec ResumableCommand) (CommandResult, error) {
	runner := d.Runner
	if runner == nil {
		runner = OSProcessRunner{}
	}
	run := Run{ID: spec.ID, WorktreePath: spec.CWD, RunnerSession: spec.ID, RunnerStatusPath: spec.StatusPath, RunnerLogPath: spec.LogPath}
	process, err := runner.Start(ctx, RunnerStart{Run: run, Argv: spec.Argv, Environment: spec.Environment})
	if err != nil {
		return CommandResult{}, err
	}
	run.RunnerPID, run.RunnerSession = process.PID, process.SessionID
	for {
		observation, observeErr := runner.Observe(ctx, run)
		if observeErr != nil {
			return CommandResult{}, observeErr
		}
		if observation.ExitStatus != nil {
			content, readErr := os.ReadFile(spec.LogPath)
			if readErr != nil {
				return CommandResult{}, readErr
			}
			return CommandResult{Argv: append([]string(nil), spec.Argv...), CWD: spec.CWD, ExitStatus: *observation.ExitStatus, Stdout: string(content)}, nil
		}
		if observation.State == "stopped_without_status" {
			return CommandResult{}, fmt.Errorf("resumable command stopped without status: %w", ErrInvalidState)
		}
		select {
		case <-ctx.Done():
			return CommandResult{}, ctx.Err()
		default:
			runtime.Gosched()
		}
	}
}

type GitAdapter struct{ Exec CommandExecutor }

type GitWorktree struct {
	Path   string `json:"path"`
	HEAD   string `json:"head"`
	Branch string `json:"branch,omitempty"`
	Locked bool   `json:"locked"`
	Reason string `json:"reason,omitempty"`
}

func (g GitAdapter) executor() CommandExecutor {
	if g.Exec == nil {
		return OSExecutor{}
	}
	return g.Exec
}

func (g GitAdapter) RevParse(ctx context.Context, repo, ref string) (string, error) {
	result, err := g.executor().Run(ctx, repo, nil, "git", "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	if result.ExitStatus != 0 {
		return "", fmt.Errorf("git rev-parse: %s", strings.TrimSpace(result.Stderr))
	}
	return strings.TrimSpace(result.Stdout), nil
}

func (g GitAdapter) ShowFile(ctx context.Context, repo, sha, path string) ([]byte, error) {
	if err := validateRelative(path); err != nil {
		return nil, err
	}
	result, err := g.executor().Run(ctx, repo, nil, "git", "show", sha+":"+filepath.ToSlash(path))
	if err != nil {
		return nil, err
	}
	if result.ExitStatus != 0 {
		return nil, fmt.Errorf("git show config: %s", strings.TrimSpace(result.Stderr))
	}
	return []byte(result.Stdout), nil
}

func (g GitAdapter) AddWorktree(ctx context.Context, repo string, run Run) error {
	actual, err := g.ListWorktrees(ctx, repo)
	if err != nil {
		return err
	}
	for _, worktree := range actual {
		if filepath.Clean(worktree.Path) == filepath.Clean(run.WorktreePath) {
			if worktree.HEAD != run.BaseSHA || worktree.Branch != "refs/heads/"+run.Branch {
				return fmt.Errorf("existing worktree differs from desired state: %w", ErrInvalidState)
			}
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(run.WorktreePath), 0o755); err != nil {
		return err
	}
	result, err := g.executor().Run(ctx, repo, nil, "git", "worktree", "add", "-b", run.Branch, run.WorktreePath, run.BaseSHA)
	if err != nil {
		return err
	}
	if result.ExitStatus != 0 {
		// A crash may leave the branch while the worktree was never registered.
		branchSHA, branchErr := g.RevParse(ctx, repo, run.Branch)
		if branchErr != nil || branchSHA != run.BaseSHA {
			return fmt.Errorf("git worktree add: %s", strings.TrimSpace(result.Stderr))
		}
		result, err = g.executor().Run(ctx, repo, nil, "git", "worktree", "add", run.WorktreePath, run.Branch)
		if err != nil {
			return err
		}
		if result.ExitStatus != 0 {
			return fmt.Errorf("git worktree resume: %s", strings.TrimSpace(result.Stderr))
		}
	}
	lock, err := g.executor().Run(ctx, repo, nil, "git", "worktree", "lock", "--reason", "nagi run "+run.ID, run.WorktreePath)
	if err != nil {
		return err
	}
	if lock.ExitStatus != 0 {
		return fmt.Errorf("git worktree lock: %s", strings.TrimSpace(lock.Stderr))
	}
	return nil
}

func (g GitAdapter) AddDetachedWorktree(ctx context.Context, repo, path, sha, reason string) error {
	actual, err := g.ListWorktrees(ctx, repo)
	if err != nil {
		return err
	}
	for _, worktree := range actual {
		if filepath.Clean(worktree.Path) == filepath.Clean(path) {
			if worktree.HEAD != sha || worktree.Branch != "" {
				return ErrInvalidState
			}
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	result, err := g.executor().Run(ctx, repo, nil, "git", "worktree", "add", "--detach", path, sha)
	if err != nil {
		return err
	}
	if result.ExitStatus != 0 {
		return fmt.Errorf("git detached worktree add: %s", strings.TrimSpace(result.Stderr))
	}
	lock, err := g.executor().Run(ctx, repo, nil, "git", "worktree", "lock", "--reason", reason, path)
	if err != nil {
		return err
	}
	if lock.ExitStatus != 0 {
		return fmt.Errorf("git worktree lock: %s", strings.TrimSpace(lock.Stderr))
	}
	return nil
}

func (g GitAdapter) ListWorktrees(ctx context.Context, repo string) ([]GitWorktree, error) {
	result, err := g.executor().Run(ctx, repo, nil, "git", "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	if result.ExitStatus != 0 {
		return nil, fmt.Errorf("git worktree list: %s", strings.TrimSpace(result.Stderr))
	}
	return ParseWorktreePorcelainZ([]byte(result.Stdout))
}

func ParseWorktreePorcelainZ(data []byte) ([]GitWorktree, error) {
	fields := bytes.Split(data, []byte{0})
	var worktrees []GitWorktree
	var current *GitWorktree
	for _, raw := range fields {
		if len(raw) == 0 {
			continue
		}
		field := string(raw)
		key, value, _ := strings.Cut(field, " ")
		switch key {
		case "worktree":
			worktrees = append(worktrees, GitWorktree{Path: value})
			current = &worktrees[len(worktrees)-1]
		case "HEAD":
			if current == nil {
				return nil, fmt.Errorf("HEAD before worktree")
			}
			current.HEAD = value
		case "branch":
			if current == nil {
				return nil, fmt.Errorf("branch before worktree")
			}
			current.Branch = value
		case "locked":
			if current == nil {
				return nil, fmt.Errorf("locked before worktree")
			}
			current.Locked, current.Reason = true, value
		case "detached", "bare", "prunable":
			if current == nil {
				return nil, fmt.Errorf("%s before worktree", key)
			}
		default:
			return nil, fmt.Errorf("unknown worktree field %q", key)
		}
	}
	for _, worktree := range worktrees {
		if worktree.Path == "" || worktree.HEAD == "" {
			return nil, fmt.Errorf("incomplete worktree record")
		}
	}
	return worktrees, nil
}

func (g GitAdapter) Dirty(ctx context.Context, path string) (bool, error) {
	result, err := g.executor().Run(ctx, path, nil, "git", "status", "--porcelain", "-z")
	if err != nil {
		return false, err
	}
	if result.ExitStatus != 0 {
		return false, fmt.Errorf("git status: %s", strings.TrimSpace(result.Stderr))
	}
	return len(result.Stdout) > 0, nil
}

func (g GitAdapter) RemoveWorktree(ctx context.Context, repo, path string) error {
	unlock, err := g.executor().Run(ctx, repo, nil, "git", "worktree", "unlock", path)
	if err != nil {
		return err
	}
	if unlock.ExitStatus != 0 && !strings.Contains(strings.ToLower(unlock.Stderr), "not locked") {
		return fmt.Errorf("git worktree unlock: %s", strings.TrimSpace(unlock.Stderr))
	}
	result, err := g.executor().Run(ctx, repo, nil, "git", "worktree", "remove", path)
	if err != nil {
		return err
	}
	if result.ExitStatus != 0 {
		return fmt.Errorf("git worktree remove: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}

func (g GitAdapter) DeleteMergedBranch(ctx context.Context, repo, branch string) error {
	result, err := g.executor().Run(ctx, repo, nil, "git", "branch", "-d", branch)
	if err != nil {
		return err
	}
	if result.ExitStatus != 0 {
		return fmt.Errorf("git branch delete: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}

func (g GitAdapter) IsAncestor(ctx context.Context, repo, ancestor, target string) (bool, error) {
	result, err := g.executor().Run(ctx, repo, nil, "git", "merge-base", "--is-ancestor", ancestor, target)
	if err != nil {
		return false, err
	}
	if result.ExitStatus == 0 {
		return true, nil
	}
	if result.ExitStatus == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base: %s", strings.TrimSpace(result.Stderr))
}

func (g GitAdapter) Push(ctx context.Context, repo, branch string) error {
	result, err := g.executor().Run(ctx, repo, nil, "git", "push", "origin", branch)
	if err != nil {
		return err
	}
	if result.ExitStatus != 0 {
		return fmt.Errorf("git push: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}

func (g GitAdapter) RemoteRepository(ctx context.Context, repo string) (string, string, error) {
	result, err := g.executor().Run(ctx, repo, nil, "git", "remote", "get-url", "origin")
	if err != nil {
		return "", "", err
	}
	if result.ExitStatus != 0 {
		return "", "", fmt.Errorf("git remote: %s", strings.TrimSpace(result.Stderr))
	}
	remote := strings.TrimSuffix(strings.TrimSpace(result.Stdout), ".git")
	var ownerRepo string
	if strings.HasPrefix(remote, "git@github.com:") {
		ownerRepo = strings.TrimPrefix(remote, "git@github.com:")
	} else if index := strings.Index(remote, "github.com/"); index >= 0 {
		ownerRepo = remote[index+len("github.com/"):]
	} else {
		return "", "", fmt.Errorf("origin is not a GitHub repository")
	}
	owner, _, ok := strings.Cut(ownerRepo, "/")
	if !ok || owner == "" {
		return "", "", fmt.Errorf("invalid GitHub remote")
	}
	return ownerRepo, owner, nil
}

func LoadRunnerConfig(content []byte) (RunnerConfig, error) {
	var config RunnerConfig
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, err
	}
	if len(config.Argv) == 0 || config.Argv[0] == "" {
		return config, fmt.Errorf("runner argv is required")
	}
	for _, seed := range config.SeedFiles {
		if err := validateRelative(seed); err != nil {
			return config, fmt.Errorf("seed %q: %w", seed, err)
		}
	}
	return config, nil
}

func CopySeedFiles(sourceRoot, destinationRoot string, seeds []string) error {
	for _, seed := range seeds {
		if err := validateRelative(seed); err != nil {
			return err
		}
		source, err := containedPath(sourceRoot, seed, true)
		if err != nil {
			return err
		}
		destination, err := containedPath(destinationRoot, seed, false)
		if err != nil {
			return err
		}
		info, err := os.Stat(source)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("seed is not a regular file: %s", seed)
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		input, err := os.Open(source)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		closeErr := errors.Join(input.Close(), output.Close())
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func validateRelative(path string) error {
	if path == "" || filepath.IsAbs(path) {
		return ErrUnsafePath
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return ErrUnsafePath
	}
	return nil
}

func containedPath(root, relative string, mustExist bool) (string, error) {
	if err := validateRelative(relative); err != nil {
		return "", err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if resolvedRoot, resolveErr := filepath.EvalSymlinks(rootAbs); resolveErr == nil {
		rootAbs = resolvedRoot
	}
	candidate := filepath.Join(rootAbs, relative)
	if mustExist {
		candidate, err = filepath.EvalSymlinks(candidate)
		if err != nil {
			return "", err
		}
	} else {
		candidate, err = resolveDestinationPath(candidate)
		if err != nil {
			return "", err
		}
	}
	rel, err := filepath.Rel(rootAbs, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrUnsafePath
	}
	return candidate, nil
}

func resolveDestinationPath(candidate string) (string, error) {
	current := filepath.Dir(candidate)
	suffix := filepath.Base(candidate)
	for {
		_, err := os.Lstat(current)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = filepath.Join(filepath.Base(current), suffix)
		current = parent
	}
	resolvedParent, err := filepath.EvalSymlinks(current)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, suffix), nil
}

type OSProcessRunner struct {
	Executable string
	IDs        IDSource
}

func (r OSProcessRunner) Start(_ context.Context, spec RunnerStart) (RunnerProcess, error) {
	executable := r.Executable
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return RunnerProcess{}, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(spec.Run.RunnerLogPath), 0o755); err != nil {
		return RunnerProcess{}, err
	}
	if err := os.MkdirAll(filepath.Dir(spec.Run.RunnerStatusPath), 0o755); err != nil {
		return RunnerProcess{}, err
	}
	markerPath := spec.Run.RunnerStatusPath + ".started"
	pidPath := spec.Run.RunnerStatusPath + ".pid"
	if content, err := os.ReadFile(pidPath); err == nil {
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(content)))
		if parseErr != nil {
			return RunnerProcess{}, parseErr
		}
		return RunnerProcess{PID: pid, SessionID: spec.Run.RunnerSession, StatusPath: spec.Run.RunnerStatusPath, LogPath: spec.Run.RunnerLogPath}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return RunnerProcess{}, err
	}
	marker, err := os.OpenFile(markerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return RunnerProcess{}, fmt.Errorf("runner start outcome is not yet observable: %w", ErrInvalidState)
		}
		return RunnerProcess{}, err
	}
	if closeErr := marker.Close(); closeErr != nil {
		return RunnerProcess{}, closeErr
	}
	logFile, err := os.OpenFile(spec.Run.RunnerLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return RunnerProcess{}, err
	}
	argv := []string{"__runner-exec", "--status", spec.Run.RunnerStatusPath, "--"}
	argv = append(argv, spec.Argv...)
	command := exec.Command(executable, argv...)
	command.Dir = spec.Run.WorktreePath
	command.Env = append([]string(nil), spec.Environment...)
	command.Stdout, command.Stderr = logFile, logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		logFile.Close()
		os.Remove(markerPath)
		return RunnerProcess{}, err
	}
	pid := command.Process.Pid
	if err := writeAtomic(pidPath, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		_ = command.Process.Release()
		logFile.Close()
		return RunnerProcess{}, err
	}
	if err := command.Process.Release(); err != nil {
		logFile.Close()
		return RunnerProcess{}, err
	}
	if err := logFile.Close(); err != nil {
		return RunnerProcess{}, err
	}
	return RunnerProcess{PID: pid, SessionID: spec.Run.RunnerSession, StatusPath: spec.Run.RunnerStatusPath, LogPath: spec.Run.RunnerLogPath}, nil
}

func (OSProcessRunner) Observe(_ context.Context, run Run) (RunnerObservation, error) {
	content, err := os.ReadFile(run.RunnerStatusPath)
	if err == nil {
		var status struct {
			ExitStatus int `json:"exitStatus"`
		}
		if err := json.Unmarshal(content, &status); err != nil {
			return RunnerObservation{}, err
		}
		state := "succeeded"
		if status.ExitStatus != 0 {
			state = "failed"
		}
		return RunnerObservation{State: state, ExitStatus: &status.ExitStatus}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return RunnerObservation{}, err
	}
	if run.RunnerPID == 0 {
		return RunnerObservation{State: "not_started"}, nil
	}
	process, err := os.FindProcess(run.RunnerPID)
	if err != nil {
		return RunnerObservation{}, err
	}
	if err := process.Signal(syscall.Signal(0)); err == nil {
		return RunnerObservation{State: "running"}, nil
	}
	return RunnerObservation{State: "stopped_without_status"}, nil
}

func (OSProcessRunner) Cancel(_ context.Context, run Run) error {
	if run.RunnerPID == 0 {
		return nil
	}
	if err := syscall.Kill(-run.RunnerPID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func RunWrappedProcess(statusPath string, argv []string) error {
	if len(argv) == 0 {
		return errors.New("runner argv is empty")
	}
	command := exec.Command(argv[0], argv[1:]...)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	err := command.Run()
	exitStatus := 0
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitStatus = exitError.ExitCode()
		} else {
			exitStatus = -1
		}
	}
	content, marshalErr := json.Marshal(map[string]any{"exitStatus": exitStatus})
	if marshalErr != nil {
		return marshalErr
	}
	if writeErr := os.WriteFile(statusPath, content, 0o600); writeErr != nil {
		return writeErr
	}
	if err != nil {
		return fmt.Errorf("runner exited %s: %w", strconv.Itoa(exitStatus), err)
	}
	return nil
}

type XcodeAdapter struct{ Exec CommandExecutor }

func (x XcodeAdapter) HostStatus(ctx context.Context) HostStatus {
	executor := x.Exec
	if executor == nil {
		executor = OSExecutor{}
	}
	selection, err := executor.Run(ctx, "", nil, "xcode-select", "-p")
	if err != nil {
		return HostStatus{Component: "xcode", Reason: "xcode_select_unavailable", Argv: []string{"xcode-select", "-p"}}
	}
	if selection.ExitStatus != 0 {
		return HostStatus{Component: "xcode", Reason: "xcode_not_selected", Argv: selection.Argv}
	}
	selected := strings.TrimSpace(selection.Stdout)
	if selected == "" {
		return HostStatus{Component: "xcode", Reason: "xcode_selection_empty", Argv: selection.Argv}
	}
	version, err := executor.Run(ctx, "", []string{"DEVELOPER_DIR=" + selected, "PATH=" + os.Getenv("PATH")}, "xcodebuild", "-version")
	if err != nil {
		return HostStatus{Component: "xcode", Reason: "xcodebuild_unavailable", Selected: selected, Argv: []string{"xcodebuild", "-version"}}
	}
	if version.ExitStatus != 0 {
		return HostStatus{Component: "xcode", Reason: "xcodebuild_not_usable", Selected: selected, Argv: version.Argv}
	}
	return HostStatus{Component: "xcode", Available: true, Reason: "available", Selected: selected, Version: strings.TrimSpace(version.Stdout), Argv: version.Argv}
}

func AddDerivedDataArg(argv []string, path string) []string {
	if len(argv) == 0 || filepath.Base(argv[0]) != "xcodebuild" {
		return append([]string(nil), argv...)
	}
	for _, arg := range argv[1:] {
		if arg == "-derivedDataPath" {
			return append([]string(nil), argv...)
		}
	}
	result := append([]string(nil), argv...)
	return append(result, "-derivedDataPath", path)
}
