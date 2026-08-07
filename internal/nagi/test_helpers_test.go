package nagi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestRepository(t *testing.T) string {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "repository")
	mustMkdir(t, repository)
	runCommand(t, repository, "git", "init", "-b", "master")
	runCommand(t, repository, "git", "config", "user.email", "nagi@example.test")
	runCommand(t, repository, "git", "config", "user.name", "Nagi Test")
	mustWrite(t, filepath.Join(repository, ".nagi.json"), []byte(`{"argv":["/usr/bin/true"]}`))
	mustWrite(t, filepath.Join(repository, "README.md"), []byte("fixture\n"))
	runCommand(t, repository, "git", "add", ".")
	runCommand(t, repository, "git", "commit", "-m", "fixture")
	return repository
}

func newTestService(t *testing.T, repository string) *Service {
	t.Helper()
	stateRoot := filepath.Join(t.TempDir(), "state")
	project, _, err := Initialize(context.Background(), repository, stateRoot, ".nagi.json", FixedClock{Time: time.Unix(1, 0).UTC()}, &SequenceIDs{Values: []string{"project_test"}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := OpenService(context.Background(), project.ID, stateRoot, FixedClock{Time: time.Unix(2, 0).UTC()}, RandomIDs{})
	if err != nil {
		t.Fatal(err)
	}
	service.Runner = newFakeRunner()
	t.Cleanup(func() { service.Close() })
	return service
}

type fakeRunner struct {
	mu     sync.Mutex
	starts map[string]int
	state  string
}

func newFakeRunner() *fakeRunner { return &fakeRunner{starts: map[string]int{}, state: "succeeded"} }

func (f *fakeRunner) Start(_ context.Context, spec RunnerStart) (RunnerProcess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts[spec.Run.ID]++
	if err := os.MkdirAll(filepath.Dir(spec.Run.RunnerLogPath), 0o700); err != nil {
		return RunnerProcess{}, err
	}
	if err := os.WriteFile(spec.Run.RunnerLogPath, []byte("runner artifact"), 0o600); err != nil {
		return RunnerProcess{}, err
	}
	return RunnerProcess{PID: len(f.starts), SessionID: spec.Run.RunnerSession, StatusPath: spec.Run.RunnerStatusPath, LogPath: spec.Run.RunnerLogPath}, nil
}

func (f *fakeRunner) Observe(_ context.Context, _ Run) (RunnerObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	exit := 0
	return RunnerObservation{State: f.state, ExitStatus: &exit}, nil
}
func (f *fakeRunner) Cancel(_ context.Context, _ Run) error {
	f.mu.Lock()
	f.state = "cancelled"
	f.mu.Unlock()
	return nil
}

type fakeExecutor struct {
	mu      sync.Mutex
	results map[string]CommandResult
	calls   [][]string
}

type recordingExecutor struct {
	delegate CommandExecutor
	mu       sync.Mutex
	calls    [][]string
}

func (r *recordingExecutor) Run(ctx context.Context, cwd string, env []string, argv ...string) (CommandResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), argv...))
	r.mu.Unlock()
	return r.delegate.Run(ctx, cwd, env, argv...)
}

func (f *fakeExecutor) Run(_ context.Context, cwd string, _ []string, argv ...string) (CommandResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), argv...))
	key := argv[0]
	if result, ok := f.results[key]; ok {
		result.Argv = append([]string(nil), argv...)
		result.CWD = cwd
		return result, nil
	}
	return CommandResult{Argv: append([]string(nil), argv...), CWD: cwd}, nil
}

type fakeGitHub struct {
	mu          sync.Mutex
	observation PRObservation
	found       *PRObservation
	created     int
	ready       int
	observeErr  error
	createErr   error
	readyErr    error
}

func (f *fakeGitHub) FindPullRequest(context.Context, string, string, string, string) (*PRObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.found == nil {
		return nil, nil
	}
	copy := *f.found
	return &copy, nil
}
func (f *fakeGitHub) CreateDraftPullRequest(_ context.Context, _, _, head, target string) (PRObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	result := f.observation
	result.HeadBranch, result.TargetBranch, result.Draft = head, target, true
	if f.createErr != nil {
		err := f.createErr
		f.createErr = nil
		f.found = &result
		return PRObservation{}, err
	}
	return result, nil
}
func (f *fakeGitHub) ObservePullRequest(context.Context, string, int) (PRObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.observeErr != nil {
		err := f.observeErr
		f.observeErr = nil
		return PRObservation{}, err
	}
	return f.observation, nil
}
func (f *fakeGitHub) MarkReady(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readyErr != nil {
		err := f.readyErr
		f.readyErr = nil
		return err
	}
	f.ready++
	f.observation.Draft = false
	return nil
}

func addReadyTask(t *testing.T, service *Service, id string) Task {
	t.Helper()
	task, err := service.AddTask(context.Background(), Task{ID: id, Title: id, IntegrationLane: "master", BaseRef: "master"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func runCommand(t *testing.T, cwd string, argv ...string) string {
	t.Helper()
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = cwd
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", argv, err, output)
	}
	return string(output)
}

func mustWrite(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}
func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func decodeCLI(t *testing.T, content []byte) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(content, &result); err != nil {
		t.Fatalf("invalid JSON %q: %v", content, err)
	}
	return result
}

func eventuallyReadStatus(t *testing.T, path string) {
	t.Helper()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

var _ = fmt.Sprint
