package nagi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndependentQAUsesDetachedWorktreeAndResumesEvidenceCapture(t *testing.T) {
	repository := newTestRepository(t)
	service := newTestService(t, repository)
	addReadyTask(t, service, "qa-task")
	run, err := service.StartTask(context.Background(), "qa-task", StartOptions{Actor: "implementation"})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := service.Git.RevParse(context.Background(), run.WorktreePath, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	executor := &fakeExecutor{results: map[string]CommandResult{"verify-one": {}, "verify-two": {}}}
	service.Exec = executor
	packet := QAPacket{CandidateSHA: candidate, Xcode: "auto", Criteria: []QACriterionSpec{{Name: "criterion one", Fixture: "README.md", Argv: []string{"verify-one"}}, {Name: "criterion two", Argv: []string{"verify-two"}}}}
	report, err := service.RunQA(context.Background(), run.ID, packet, QAOptions{Actor: "qa", FaultAfter: "criterion one/process"})
	if !errors.Is(err, ErrInjectedFault) {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	report, err = service.RunQA(context.Background(), run.ID, packet, QAOptions{Actor: "qa-resume"})
	if err != nil {
		t.Fatal(err)
	}
	if report.QA.State != "passed" || report.QA.ValidatedSHA != candidate || len(report.Criteria) != 2 {
		t.Fatalf("%+v", report)
	}
	if report.QA.WorktreePath == run.WorktreePath || report.QA.DerivedDataPath == run.DerivedDataPath {
		t.Fatalf("QA isolation collapsed: %+v %+v", report.QA, run)
	}
	worktrees, err := service.Git.ListWorktrees(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	foundDetached := false
	for _, worktree := range worktrees {
		if worktree.Path == report.QA.WorktreePath && worktree.HEAD == candidate && worktree.Branch == "" {
			foundDetached = true
		}
	}
	if !foundDetached {
		t.Fatalf("detached QA worktree not found: %+v", worktrees)
	}
	if len(executor.calls) != 2 {
		t.Fatalf("completed criterion reran: calls=%v", executor.calls)
	}
	for _, criterion := range report.Criteria {
		if criterion.Status != "pass" || len(criterion.Artifacts) == 0 {
			t.Fatalf("%+v", criterion)
		}
		for _, artifact := range criterion.Artifacts {
			if _, err := os.Stat(artifact); err != nil {
				t.Fatalf("artifact %s: %v", artifact, err)
			}
		}
	}
}

func TestQAFailureAndMissingArtifactDoNotValidateSHA(t *testing.T) {
	repository := newTestRepository(t)
	service := newTestService(t, repository)
	addReadyTask(t, service, "qa-fail")
	run, err := service.StartTask(context.Background(), "qa-fail", StartOptions{Actor: "implementation"})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := service.Git.RevParse(context.Background(), run.WorktreePath, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	service.Exec = &fakeExecutor{results: map[string]CommandResult{"false-command": {ExitStatus: 1}}}
	packet := QAPacket{CandidateSHA: candidate, Criteria: []QACriterionSpec{{Name: "fails", Argv: []string{"false-command"}}, {Name: "missing artifact", Argv: []string{"true-command"}, Artifacts: []string{"missing.xcresult"}}}}
	report, err := service.RunQA(context.Background(), run.ID, packet, QAOptions{Actor: "qa"})
	if err != nil {
		t.Fatal(err)
	}
	if report.QA.State != "failed" || report.QA.ValidatedSHA != "" {
		t.Fatalf("%+v", report.QA)
	}
	for _, criterion := range report.Criteria {
		if criterion.Status != "fail" {
			t.Fatalf("%+v", criterion)
		}
	}
}

func TestQACopiesDirectoryArtifactOutsideDatabase(t *testing.T) {
	repository := newTestRepository(t)
	mustMkdir(t, filepath.Join(repository, "Result.xcresult"))
	mustWrite(t, filepath.Join(repository, "Result.xcresult", "evidence.json"), []byte(`{"passed":true}`))
	runCommand(t, repository, "git", "add", "Result.xcresult")
	runCommand(t, repository, "git", "commit", "-m", "fixture artifact")
	service := newTestService(t, repository)
	addReadyTask(t, service, "qa-directory-artifact")
	run, err := service.StartTask(context.Background(), "qa-directory-artifact", StartOptions{Actor: "implementation"})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := service.Git.RevParse(context.Background(), run.WorktreePath, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	service.Exec = &fakeExecutor{results: map[string]CommandResult{"verify": {}}}
	report, err := service.RunQA(context.Background(), run.ID, QAPacket{CandidateSHA: candidate, Criteria: []QACriterionSpec{{Name: "bundle", Argv: []string{"verify"}, Artifacts: []string{"Result.xcresult"}}}}, QAOptions{Actor: "qa"})
	if err != nil {
		t.Fatal(err)
	}
	if report.QA.State != "passed" || len(report.Criteria) != 1 || len(report.Criteria[0].Artifacts) != 2 {
		t.Fatalf("%+v", report)
	}
	copiedBundle := report.Criteria[0].Artifacts[1]
	if _, err := os.Stat(filepath.Join(copiedBundle, "evidence.json")); err != nil {
		t.Fatalf("directory artifact was not copied: %v", err)
	}
}

func TestPreparePullRequestReusesExistingDraftAndIsIdempotent(t *testing.T) {
	repository := newTestRepository(t)
	runCommand(t, repository, "git", "remote", "add", "origin", "https://github.com/acme/nagi-smoke.git")
	service := newTestService(t, repository)
	addReadyTask(t, service, "pr-task")
	run, qa := makePassedQA(t, service, "pr-task")
	observation := PRObservation{Number: 7, NodeID: "PR_node", Draft: true, TargetBranch: "master", HeadBranch: run.Branch, HeadSHA: qa.ValidatedSHA, CIState: "pending", ConflictState: "none", ReviewState: "none"}
	github := &fakeGitHub{observation: observation, found: &observation}
	service.GitHub = github
	interceptor := &pushInterceptExecutor{delegate: OSExecutor{}}
	service.Git = GitAdapter{Exec: interceptor}
	first, err := service.PreparePullRequest(context.Background(), run.ID, "master", "publisher")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.PreparePullRequest(context.Background(), run.ID, "master", "publisher-retry")
	if err != nil {
		t.Fatal(err)
	}
	if first.Number != second.Number || first.Number != 7 || github.created != 0 {
		t.Fatalf("first=%+v second=%+v created=%d", first, second, github.created)
	}
	if interceptor.pushes != 2 {
		t.Fatalf("expected idempotent official git push retries, got %d", interceptor.pushes)
	}
}

func TestPreparePullRequestRecoversAmbiguousCreateWithoutDuplicate(t *testing.T) {
	repository := newTestRepository(t)
	runCommand(t, repository, "git", "remote", "add", "origin", "https://github.com/acme/nagi-smoke.git")
	service := newTestService(t, repository)
	addReadyTask(t, service, "ambiguous-create")
	run, qa := makePassedQA(t, service, "ambiguous-create")
	observation := PRObservation{Number: 71, NodeID: "PR_ambiguous", Draft: true, TargetBranch: "master", HeadBranch: run.Branch, HeadSHA: qa.ValidatedSHA, CIState: "pending", ConflictState: "none", ReviewState: "none"}
	github := &fakeGitHub{observation: observation, createErr: errors.New("connection lost after create")}
	service.GitHub = github
	service.Git = GitAdapter{Exec: &pushInterceptExecutor{delegate: OSExecutor{}}}
	if _, err := service.PreparePullRequest(context.Background(), run.ID, "master", "publisher"); err == nil {
		t.Fatal("ambiguous network failure was not surfaced")
	}
	pr, err := service.PreparePullRequest(context.Background(), run.ID, "master", "publisher-retry")
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != observation.Number || github.created != 1 {
		t.Fatalf("pr=%+v creates=%d", pr, github.created)
	}
}

func TestUndraftRequiresSameValidatedSHAAllCIAndArtifacts(t *testing.T) {
	repository := newTestRepository(t)
	runCommand(t, repository, "git", "remote", "add", "origin", "https://github.com/acme/nagi-smoke.git")
	service := newTestService(t, repository)
	addReadyTask(t, service, "ready-pr")
	run, qa := makePassedQA(t, service, "ready-pr")
	observation := PRObservation{Number: 8, NodeID: "PR_ready", Draft: true, TargetBranch: "master", HeadBranch: run.Branch, HeadSHA: qa.ValidatedSHA, CIState: "passed", ConflictState: "none", ReviewState: "approved"}
	github := &fakeGitHub{observation: observation}
	service.GitHub = github
	seedPullRequest(t, service, run, observation)
	decision, err := service.UndraftPullRequest(context.Background(), run.ID, "release")
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Ready || github.ready != 1 || decision.PR.ExpectedState != "ready" {
		t.Fatalf("decision=%+v ready calls=%d", decision, github.ready)
	}
}

func TestUndraftBlocksMissingArtifactAndRetriesNetworkFailures(t *testing.T) {
	repository := newTestRepository(t)
	runCommand(t, repository, "git", "remote", "add", "origin", "https://github.com/acme/nagi-smoke.git")
	service := newTestService(t, repository)
	addReadyTask(t, service, "resilient-pr")
	run, qa := makePassedQA(t, service, "resilient-pr")
	observation := PRObservation{Number: 81, NodeID: "PR_resilient", Draft: true, TargetBranch: "master", HeadBranch: run.Branch, HeadSHA: qa.ValidatedSHA, CIState: "passed", ConflictState: "none", ReviewState: "approved"}
	github := &fakeGitHub{observation: observation}
	service.GitHub = github
	seedPullRequest(t, service, run, observation)
	criteria, err := service.Store.QACriteria(context.Background(), qa.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(criteria[0].Artifacts[0]); err != nil {
		t.Fatal(err)
	}
	decision, err := service.UndraftPullRequest(context.Background(), run.ID, "release")
	if !errors.Is(err, ErrUndraftBlocked) || !contains(decision.Reasons, "required_artifact_missing") || github.ready != 0 {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	mustWrite(t, criteria[0].Artifacts[0], []byte("restored evidence"))
	github.observeErr = errors.New("network unavailable")
	if _, err := service.UndraftPullRequest(context.Background(), run.ID, "release-retry"); err == nil {
		t.Fatal("network failure was not surfaced")
	}
	github.readyErr = errors.New("connection lost before response")
	if _, err := service.UndraftPullRequest(context.Background(), run.ID, "release-retry"); err == nil {
		t.Fatal("mark-ready network failure was not surfaced")
	}
	decision, err = service.UndraftPullRequest(context.Background(), run.ID, "release-resume")
	if err != nil || !decision.Ready || github.ready != 1 {
		t.Fatalf("decision=%+v err=%v ready=%d", decision, err, github.ready)
	}
}

func TestPullRequestHeadChangeInvalidatesQAAndUnchangedSyncCreatesNoEvent(t *testing.T) {
	repository := newTestRepository(t)
	runCommand(t, repository, "git", "remote", "add", "origin", "https://github.com/acme/nagi-smoke.git")
	service := newTestService(t, repository)
	addReadyTask(t, service, "changed-pr")
	run, qa := makePassedQA(t, service, "changed-pr")
	observation := PRObservation{Number: 9, NodeID: "PR_changed", Draft: true, TargetBranch: "master", HeadBranch: run.Branch, HeadSHA: qa.ValidatedSHA, CIState: "pending", ConflictState: "none", ReviewState: "none"}
	github := &fakeGitHub{observation: observation}
	service.GitHub = github
	seedPullRequest(t, service, run, observation)
	before, err := service.Store.Events(context.Background(), service.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, changed, err := service.SyncPullRequest(context.Background(), run.ID, "poll")
	if err != nil {
		t.Fatal(err)
	}
	after, err := service.Store.Events(context.Background(), service.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if changed || len(after) != len(before) {
		t.Fatalf("unchanged poll emitted state: changed=%v before=%d after=%d", changed, len(before), len(after))
	}
	github.observation.HeadSHA = "ffffffffffffffffffffffffffffffffffffffff"
	decision, err := service.UndraftPullRequest(context.Background(), run.ID, "poll")
	if !errors.Is(err, ErrUndraftBlocked) {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	if github.ready != 0 || !contains(decision.Reasons, "head_sha_not_validated") || !contains(decision.Reasons, "qa_not_passed") {
		t.Fatalf("decision=%+v ready=%d", decision, github.ready)
	}
	invalidated, err := service.Store.LatestQARun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if invalidated.State != "invalidated" || invalidated.ValidatedSHA != "" {
		t.Fatalf("%+v", invalidated)
	}
}

func TestUndraftKeepsDraftWhenCIOrConflictBlocks(t *testing.T) {
	for _, test := range []struct{ name, ci, conflict, reason string }{
		{"ci pending", "pending", "none", "required_ci_not_passed"},
		{"conflict unknown", "passed", "unknown", "merge_conflict_or_unknown"},
		{"merge conflict", "passed", "conflict", "merge_conflict_or_unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := newTestRepository(t)
			runCommand(t, repository, "git", "remote", "add", "origin", "https://github.com/acme/nagi-smoke.git")
			service := newTestService(t, repository)
			addReadyTask(t, service, "blocked-pr")
			run, qa := makePassedQA(t, service, "blocked-pr")
			observation := PRObservation{Number: 10, NodeID: "PR_blocked", Draft: true, TargetBranch: "master", HeadBranch: run.Branch, HeadSHA: qa.ValidatedSHA, CIState: test.ci, ConflictState: test.conflict, ReviewState: "none"}
			github := &fakeGitHub{observation: observation}
			service.GitHub = github
			seedPullRequest(t, service, run, observation)
			decision, err := service.UndraftPullRequest(context.Background(), run.ID, "release")
			if !errors.Is(err, ErrUndraftBlocked) || !contains(decision.Reasons, test.reason) || github.ready != 0 {
				t.Fatalf("decision=%+v err=%v ready=%d", decision, err, github.ready)
			}
		})
	}
}

func TestGHTransportEvaluatesEveryRequiredStatusAndCheckRun(t *testing.T) {
	executor := &endpointExecutor{responses: map[string]CommandResult{
		"required_status_checks": {Stdout: `{"contexts":["lint"],"checks":[{"context":"tests","app_id":42}]}`},
		"/statuses":              {Stdout: `[[{"context":"lint","state":"success"}]]`},
		"/check-runs":            {Stdout: `[{"check_runs":[{"name":"tests","status":"completed","conclusion":"success","app":{"id":42}}]}]`},
	}}
	adapter := GHAdapter{Exec: executor}
	state, err := adapter.requiredCIState(context.Background(), "acme/project", "master", "abc")
	if err != nil || state != "passed" {
		t.Fatalf("state=%s err=%v calls=%v", state, err, executor.calls)
	}
	executor.responses["/check-runs"] = CommandResult{Stdout: `[{"check_runs":[{"name":"tests","status":"in_progress","conclusion":null,"app":{"id":42}}]}]`}
	state, err = adapter.requiredCIState(context.Background(), "acme/project", "master", "abc")
	if err != nil || state != "pending" {
		t.Fatalf("state=%s err=%v", state, err)
	}
	executor.responses["/check-runs"] = CommandResult{Stdout: `[{"check_runs":[{"name":"tests","status":"completed","conclusion":"failure","app":{"id":42}}]}]`}
	state, err = adapter.requiredCIState(context.Background(), "acme/project", "master", "abc")
	if err != nil || state != "failed" {
		t.Fatalf("state=%s err=%v", state, err)
	}
}

func TestGHTransportClassifiesUnprotectedCheckRuns(t *testing.T) {
	for _, test := range []struct {
		name, checkRun, want string
	}{
		{"queued is pending", `{"name":"nix-environment","status":"queued","conclusion":null,"app":{"id":15368}}`, "pending"},
		{"in progress is pending", `{"name":"nix-environment","status":"in_progress","conclusion":null,"app":{"id":15368}}`, "pending"},
		{"success passes", `{"name":"nix-environment","status":"completed","conclusion":"success","app":{"id":15368}}`, "passed"},
		{"failure fails", `{"name":"nix-environment","status":"completed","conclusion":"failure","app":{"id":15368}}`, "failed"},
		{"cancelled fails", `{"name":"nix-environment","status":"completed","conclusion":"cancelled","app":{"id":15368}}`, "failed"},
		{"timed out fails", `{"name":"nix-environment","status":"completed","conclusion":"timed_out","app":{"id":15368}}`, "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor := &endpointExecutor{responses: map[string]CommandResult{
				"pulls/2/reviews":        {Stdout: `[]`},
				"pulls/2":                {Stdout: `{"number":2,"node_id":"PR_pending","draft":true,"mergeable":true,"mergeable_state":"clean","base":{"ref":"main"},"head":{"ref":"feature","sha":"abc"}}`},
				"required_status_checks": {ExitStatus: 1, Stderr: "gh: Branch not protected (HTTP 404)"},
				"/statuses":              {Stdout: `[[]]`},
				"/check-runs":            {Stdout: `[{"check_runs":[` + test.checkRun + `]}]`},
			}}

			observation, err := (GHAdapter{Exec: executor}).ObservePullRequest(context.Background(), "acme/project", 2)
			if err != nil {
				t.Fatal(err)
			}
			if observation.CIState != test.want {
				t.Fatalf("ciState=%s want %s; calls=%v", observation.CIState, test.want, executor.calls)
			}
		})
	}
}

func TestUndraftWaitsForEveryRequiredCheckRunToSucceed(t *testing.T) {
	repository := newTestRepository(t)
	runCommand(t, repository, "git", "remote", "add", "origin", "https://github.com/acme/nagi-smoke.git")
	service := newTestService(t, repository)
	addReadyTask(t, service, "required-checks")
	run, qa := makePassedQA(t, service, "required-checks")
	observation := PRObservation{Number: 12, NodeID: "PR_required", Draft: true, TargetBranch: "master", HeadBranch: run.Branch, HeadSHA: qa.ValidatedSHA, CIState: "pending", ConflictState: "none", ReviewState: "none"}
	seedPullRequest(t, service, run, observation)
	executor := &endpointExecutor{responses: map[string]CommandResult{
		"pulls/12/reviews":       {Stdout: `[]`},
		"pulls/12":               {Stdout: fmt.Sprintf(`{"number":12,"node_id":"PR_required","draft":true,"mergeable":true,"mergeable_state":"clean","base":{"ref":"master"},"head":{"ref":%q,"sha":%q}}`, run.Branch, qa.ValidatedSHA)},
		"required_status_checks": {Stdout: `{"contexts":[],"checks":[{"context":"lint","app_id":42},{"context":"tests","app_id":42}]}`},
		"/statuses":              {Stdout: `[[]]`},
		"/check-runs":            {Stdout: `[{"check_runs":[{"name":"lint","status":"completed","conclusion":"success","app":{"id":42}},{"name":"tests","status":"in_progress","conclusion":null,"app":{"id":42}}]}]`},
	}}
	service.GitHub = GHAdapter{Exec: executor}

	decision, err := service.UndraftPullRequest(context.Background(), run.ID, "release")
	if !errors.Is(err, ErrUndraftBlocked) || !contains(decision.Reasons, "required_ci_not_passed") || countEndpointCalls(executor.calls, "graphql") != 0 {
		t.Fatalf("decision=%+v err=%v calls=%v", decision, err, executor.calls)
	}

	executor.responses["/check-runs"] = CommandResult{Stdout: `[{"check_runs":[{"name":"lint","status":"completed","conclusion":"success","app":{"id":42}},{"name":"tests","status":"completed","conclusion":"success","app":{"id":42}}]}]`}
	decision, err = service.UndraftPullRequest(context.Background(), run.ID, "release")
	if err != nil || !decision.Ready || countEndpointCalls(executor.calls, "graphql") != 1 {
		t.Fatalf("decision=%+v err=%v calls=%v", decision, err, executor.calls)
	}
}

func makePassedQA(t *testing.T, service *Service, taskID string) (Run, QARun) {
	t.Helper()
	run, err := service.StartTask(context.Background(), taskID, StartOptions{Actor: "implementation"})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := service.Git.RevParse(context.Background(), run.WorktreePath, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	service.Exec = &fakeExecutor{results: map[string]CommandResult{"qa-pass": {}}}
	report, err := service.RunQA(context.Background(), run.ID, QAPacket{CandidateSHA: candidate, Criteria: []QACriterionSpec{{Name: "acceptance", Argv: []string{"qa-pass"}}}}, QAOptions{Actor: "qa"})
	if err != nil {
		t.Fatal(err)
	}
	return run, report.QA
}

func seedPullRequest(t *testing.T, service *Service, run Run, observation PRObservation) {
	t.Helper()
	op := Operation{ID: service.IDs.NewID("operation"), RunID: run.ID, Kind: "pull_request", DesiredState: "ready", ObservedState: "planned", CreatedAt: service.Clock.Now(), UpdatedAt: service.Clock.Now()}
	var err error
	op, err = service.Store.EnsureOperation(context.Background(), op)
	if err != nil {
		t.Fatal(err)
	}
	pr := pullRequestFromObservation(run.ID, observation, "ready")
	if _, _, err := service.Store.UpsertPullRequest(context.Background(), service.Project.ID, "test", op.ID, pr); err != nil {
		t.Fatal(err)
	}
}

type pushInterceptExecutor struct {
	delegate CommandExecutor
	pushes   int
}

type endpointExecutor struct {
	responses map[string]CommandResult
	calls     [][]string
}

func (e *endpointExecutor) Run(_ context.Context, cwd string, _ []string, argv ...string) (CommandResult, error) {
	e.calls = append(e.calls, append([]string(nil), argv...))
	joined := strings.Join(argv, " ")
	for key, response := range e.responses {
		if strings.Contains(joined, key) {
			response.Argv, response.CWD = append([]string(nil), argv...), cwd
			return response, nil
		}
	}
	return CommandResult{Argv: append([]string(nil), argv...), CWD: cwd}, nil
}

func (p *pushInterceptExecutor) Run(ctx context.Context, cwd string, env []string, argv ...string) (CommandResult, error) {
	if len(argv) >= 2 && argv[0] == "git" && argv[1] == "push" {
		p.pushes++
		return CommandResult{Argv: append([]string(nil), argv...), CWD: cwd}, nil
	}
	return p.delegate.Run(ctx, cwd, env, argv...)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func countEndpointCalls(calls [][]string, part string) int {
	count := 0
	for _, call := range calls {
		if strings.Contains(strings.Join(call, " "), part) {
			count++
		}
	}
	return count
}

var _ = filepath.Join
