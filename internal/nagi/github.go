package nagi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

type GHAdapter struct{ Exec CommandExecutor }

type ghPull struct {
	Number         int    `json:"number"`
	NodeID         string `json:"node_id"`
	Draft          bool   `json:"draft"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	Base           struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

func (g GHAdapter) executor() CommandExecutor {
	if g.Exec == nil {
		return OSExecutor{}
	}
	return g.Exec
}

func (g GHAdapter) FindPullRequest(ctx context.Context, repository, headOwner, headBranch, targetBranch string) (*PRObservation, error) {
	result, err := g.executor().Run(ctx, "", nil, "gh", "api", "--method", "GET", "repos/"+repository+"/pulls", "-f", "state=open", "-f", "head="+headOwner+":"+headBranch, "-f", "base="+targetBranch)
	if err != nil {
		return nil, err
	}
	if result.ExitStatus != 0 {
		return nil, fmt.Errorf("gh api pull search: %s", strings.TrimSpace(result.Stderr))
	}
	var pulls []ghPull
	if err := json.Unmarshal([]byte(result.Stdout), &pulls); err != nil {
		return nil, err
	}
	if len(pulls) == 0 {
		return nil, nil
	}
	observation := pullObservation(pulls[0], "pending", conflictState(pulls[0]), "pending")
	return &observation, nil
}

func (g GHAdapter) CreateDraftPullRequest(ctx context.Context, repository, title, headBranch, targetBranch string) (PRObservation, error) {
	result, err := g.executor().Run(ctx, "", nil, "gh", "api", "--method", "POST", "repos/"+repository+"/pulls", "-f", "title="+title, "-f", "head="+headBranch, "-f", "base="+targetBranch, "-F", "draft=true")
	if err != nil {
		return PRObservation{}, err
	}
	if result.ExitStatus != 0 {
		return PRObservation{}, fmt.Errorf("gh api create draft: %s", strings.TrimSpace(result.Stderr))
	}
	var pull ghPull
	if err := json.Unmarshal([]byte(result.Stdout), &pull); err != nil {
		return PRObservation{}, err
	}
	return pullObservation(pull, "pending", conflictState(pull), "pending"), nil
}

func (g GHAdapter) ObservePullRequest(ctx context.Context, repository string, number int) (PRObservation, error) {
	result, err := g.executor().Run(ctx, "", nil, "gh", "api", "repos/"+repository+"/pulls/"+fmt.Sprint(number))
	if err != nil {
		return PRObservation{}, err
	}
	if result.ExitStatus != 0 {
		return PRObservation{}, fmt.Errorf("gh api pull: %s", strings.TrimSpace(result.Stderr))
	}
	var pull ghPull
	if err := json.Unmarshal([]byte(result.Stdout), &pull); err != nil {
		return PRObservation{}, err
	}
	ciState, err := g.requiredCIState(ctx, repository, pull.Base.Ref, pull.Head.SHA)
	if err != nil {
		return PRObservation{}, err
	}
	reviewsState := "none"
	reviews, err := g.executor().Run(ctx, "", nil, "gh", "api", "repos/"+repository+"/pulls/"+fmt.Sprint(number)+"/reviews")
	if err != nil {
		return PRObservation{}, err
	}
	if reviews.ExitStatus == 0 {
		var entries []struct {
			State string `json:"state"`
		}
		if json.Unmarshal([]byte(reviews.Stdout), &entries) == nil {
			for _, entry := range entries {
				switch entry.State {
				case "CHANGES_REQUESTED":
					reviewsState = "changes_requested"
				case "APPROVED":
					if reviewsState != "changes_requested" {
						reviewsState = "approved"
					}
				}
			}
		}
	}
	return pullObservation(pull, ciState, conflictState(pull), reviewsState), nil
}

type requiredCheck struct {
	Context string `json:"context"`
	AppID   int64  `json:"app_id"`
}

func (g GHAdapter) requiredCIState(ctx context.Context, repository, targetBranch, sha string) (string, error) {
	protection, err := g.executor().Run(ctx, "", nil, "gh", "api", "repos/"+repository+"/branches/"+url.PathEscape(targetBranch)+"/protection/required_status_checks")
	if err != nil {
		return "", err
	}
	var rules struct {
		Contexts []string        `json:"contexts"`
		Checks   []requiredCheck `json:"checks"`
	}
	observeAll := false
	if protection.ExitStatus != 0 {
		lower := strings.ToLower(protection.Stderr)
		if strings.Contains(lower, "404") || strings.Contains(lower, "not found") {
			observeAll = true
		} else {
			return "", fmt.Errorf("gh api required checks: %s", strings.TrimSpace(protection.Stderr))
		}
	} else if err := json.Unmarshal([]byte(protection.Stdout), &rules); err != nil {
		return "", err
	}
	required := make(map[string]requiredCheck)
	for _, contextName := range rules.Contexts {
		required[contextName] = requiredCheck{Context: contextName}
	}
	for _, check := range rules.Checks {
		required[check.Context] = check
	}
	if !observeAll && len(required) == 0 {
		return "passed", nil
	}
	statusesResult, err := g.executor().Run(ctx, "", nil, "gh", "api", "--paginate", "--slurp", "repos/"+repository+"/commits/"+sha+"/statuses")
	if err != nil {
		return "", err
	}
	if statusesResult.ExitStatus != 0 {
		return "", fmt.Errorf("gh api commit statuses: %s", strings.TrimSpace(statusesResult.Stderr))
	}
	var statusPages [][]struct {
		Context string `json:"context"`
		State   string `json:"state"`
	}
	if err := json.Unmarshal([]byte(statusesResult.Stdout), &statusPages); err != nil {
		return "", err
	}
	statuses := map[string]string{}
	for _, page := range statusPages {
		for _, status := range page {
			if _, exists := statuses[status.Context]; !exists {
				statuses[status.Context] = status.State
			}
		}
	}
	checksResult, err := g.executor().Run(ctx, "", nil, "gh", "api", "--paginate", "--slurp", "repos/"+repository+"/commits/"+sha+"/check-runs")
	if err != nil {
		return "", err
	}
	if checksResult.ExitStatus != 0 {
		return "", fmt.Errorf("gh api check runs: %s", strings.TrimSpace(checksResult.Stderr))
	}
	var checkPages []struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			App        struct {
				ID int64 `json:"id"`
			} `json:"app"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal([]byte(checksResult.Stdout), &checkPages); err != nil {
		return "", err
	}
	checks := map[string]struct {
		status, conclusion string
		appID              int64
	}{}
	for _, page := range checkPages {
		for _, check := range page.CheckRuns {
			if _, exists := checks[check.Name]; !exists {
				checks[check.Name] = struct {
					status, conclusion string
					appID              int64
				}{check.Status, check.Conclusion, check.App.ID}
			}
		}
	}
	state := "passed"
	if observeAll {
		for _, status := range statuses {
			state = mergeCIState(state, commitStatusCIState(status))
		}
		for _, check := range checks {
			state = mergeCIState(state, checkRunCIState(check.status, check.conclusion))
		}
		return state, nil
	}
	for name, requirement := range required {
		if status, ok := statuses[name]; ok {
			state = mergeCIState(state, commitStatusCIState(status))
			continue
		}
		if check, ok := checks[name]; ok && (requirement.AppID == 0 || requirement.AppID == check.appID) {
			state = mergeCIState(state, checkRunCIState(check.status, check.conclusion))
			continue
		}
		state = "pending"
	}
	return state, nil
}

func commitStatusCIState(status string) string {
	switch status {
	case "success":
		return "passed"
	case "failure", "error":
		return "failed"
	default:
		return "pending"
	}
}

func checkRunCIState(status, conclusion string) string {
	if status != "completed" {
		return "pending"
	}
	switch conclusion {
	case "success", "neutral", "skipped":
		return "passed"
	case "failure", "cancelled", "timed_out", "action_required", "stale":
		return "failed"
	default:
		return "pending"
	}
}

func mergeCIState(current, observed string) string {
	if current == "failed" || observed == "failed" {
		return "failed"
	}
	if current == "pending" || observed == "pending" {
		return "pending"
	}
	return "passed"
}

func (g GHAdapter) MarkReady(ctx context.Context, repository, nodeID string) error {
	query := `mutation($pullRequestId:ID!){markPullRequestReadyForReview(input:{pullRequestId:$pullRequestId}){pullRequest{id isDraft}}}`
	result, err := g.executor().Run(ctx, "", nil, "gh", "api", "graphql", "-f", "query="+query, "-f", "pullRequestId="+nodeID)
	if err != nil {
		return err
	}
	if result.ExitStatus != 0 {
		return fmt.Errorf("gh api mark ready: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}

func (g GHAdapter) HostStatus(ctx context.Context) HostStatus {
	result, err := g.executor().Run(ctx, "", nil, "gh", "auth", "status", "--active")
	if err != nil {
		return HostStatus{Component: "github", Reason: "gh_unavailable", Argv: []string{"gh", "auth", "status", "--active"}}
	}
	if result.ExitStatus != 0 {
		return HostStatus{Component: "github", Reason: "gh_not_authenticated", Argv: result.Argv}
	}
	return HostStatus{Component: "github", Available: true, Reason: "available", Version: "authenticated", Argv: result.Argv}
}

func pullObservation(pull ghPull, ci, conflict, review string) PRObservation {
	return PRObservation{Number: pull.Number, NodeID: pull.NodeID, Draft: pull.Draft, TargetBranch: pull.Base.Ref, HeadBranch: pull.Head.Ref, HeadSHA: pull.Head.SHA, CIState: ci, ConflictState: conflict, ReviewState: review}
}

func conflictState(pull ghPull) string {
	if pull.Mergeable != nil && !*pull.Mergeable {
		return "conflict"
	}
	if pull.MergeableState == "dirty" {
		return "conflict"
	}
	if pull.Mergeable == nil {
		return "unknown"
	}
	return "none"
}

func (s *Service) PreparePullRequest(ctx context.Context, runID, targetBranch, actor string) (PullRequest, error) {
	run, err := s.Store.Run(ctx, runID)
	if err != nil {
		return PullRequest{}, err
	}
	task, err := s.Store.Task(ctx, run.TaskID)
	if err != nil {
		return PullRequest{}, err
	}
	qa, err := s.Store.LatestQARun(ctx, run.ID)
	if err != nil || qa.State != "passed" || qa.ValidatedSHA == "" {
		return PullRequest{}, fmt.Errorf("independent QA has not passed: %w", ErrInvalidState)
	}
	headSHA, err := s.Git.RevParse(ctx, s.Project.Repository, run.Branch)
	if err != nil {
		return PullRequest{}, err
	}
	if headSHA != qa.ValidatedSHA {
		return PullRequest{}, fmt.Errorf("branch head differs from validated SHA: %w", ErrInvalidState)
	}
	if targetBranch == "" {
		targetBranch = task.BaseRef
	}
	op := Operation{ID: s.IDs.NewID("operation"), RunID: run.ID, Kind: "pull_request", DesiredState: "ready", ObservedState: "planned", CreatedAt: s.Clock.Now(), UpdatedAt: s.Clock.Now()}
	op, err = s.Store.EnsureOperation(ctx, op)
	if err != nil {
		return PullRequest{}, err
	}
	if err := s.Git.Push(ctx, s.Project.Repository, run.Branch); err != nil {
		return PullRequest{}, err
	}
	if err := s.Store.CompleteStep(ctx, s.Project.ID, actor, op, "branch_pushed", nil, map[string]string{"branch": run.Branch, "sha": headSHA}); err != nil {
		return PullRequest{}, err
	}
	repository, owner, err := s.Git.RemoteRepository(ctx, s.Project.Repository)
	if err != nil {
		return PullRequest{}, err
	}
	observation, err := s.GitHub.FindPullRequest(ctx, repository, owner, run.Branch, targetBranch)
	if err != nil {
		return PullRequest{}, err
	}
	if observation == nil {
		created, err := s.GitHub.CreateDraftPullRequest(ctx, repository, task.Title, run.Branch, targetBranch)
		if err != nil {
			return PullRequest{}, err
		}
		observation = &created
	}
	pr := pullRequestFromObservation(run.ID, *observation, "ready")
	pr, _, err = s.Store.UpsertPullRequest(ctx, s.Project.ID, actor, op.ID, pr)
	if err != nil {
		return PullRequest{}, err
	}
	if err := s.Store.CompleteStep(ctx, s.Project.ID, actor, op, "draft_pr_reconciled", nil, pr); err != nil {
		return PullRequest{}, err
	}
	return pr, nil
}

func (s *Service) SyncPullRequest(ctx context.Context, runID, actor string) (PullRequest, bool, error) {
	pr, err := s.Store.PullRequest(ctx, runID)
	if err != nil {
		return PullRequest{}, false, err
	}
	repository, _, err := s.Git.RemoteRepository(ctx, s.Project.Repository)
	if err != nil {
		return PullRequest{}, false, err
	}
	observation, err := s.GitHub.ObservePullRequest(ctx, repository, pr.Number)
	if err != nil {
		return PullRequest{}, false, err
	}
	updated := pullRequestFromObservation(runID, observation, pr.ExpectedState)
	op, err := s.Store.Operation(ctx, runID, "pull_request")
	if err != nil {
		return PullRequest{}, false, err
	}
	updated, changed, err := s.Store.UpsertPullRequest(ctx, s.Project.ID, actor, op.ID, updated)
	if err != nil {
		return PullRequest{}, false, err
	}
	qa, qaErr := s.Store.LatestQARun(ctx, runID)
	if qaErr == nil && qa.ValidatedSHA != "" && observation.HeadSHA != qa.ValidatedSHA && qa.State != "invalidated" {
		if _, updateErr := s.Store.UpdateQARun(ctx, qa, "invalidated", qa.SelectedXcode, "", s.Project.ID, actor, op.ID); updateErr != nil {
			return PullRequest{}, changed, updateErr
		}
	}
	return updated, changed, nil
}

func (s *Service) UndraftPullRequest(ctx context.Context, runID, actor string) (UndraftDecision, error) {
	pr, _, err := s.SyncPullRequest(ctx, runID, actor)
	if err != nil {
		return UndraftDecision{}, err
	}
	qa, err := s.Store.LatestQARun(ctx, runID)
	if err != nil {
		return UndraftDecision{}, err
	}
	var reasons []string
	if qa.State != "passed" || qa.ValidatedSHA == "" {
		reasons = append(reasons, "qa_not_passed")
	}
	if pr.HeadSHA != qa.ValidatedSHA {
		reasons = append(reasons, "head_sha_not_validated")
	}
	if pr.CIState != "passed" {
		reasons = append(reasons, "required_ci_not_passed")
	}
	if pr.ConflictState != "none" {
		reasons = append(reasons, "merge_conflict_or_unknown")
	}
	criteria, err := s.Store.QACriteria(ctx, qa.ID)
	if err != nil {
		return UndraftDecision{}, err
	}
	for _, criterion := range criteria {
		for _, artifact := range criterion.Artifacts {
			if _, statErr := os.Stat(artifact); statErr != nil {
				reasons = append(reasons, "required_artifact_missing")
				break
			}
		}
	}
	decision := UndraftDecision{Ready: len(reasons) == 0, Reasons: uniqueStrings(reasons), PR: pr}
	if !decision.Ready {
		return decision, ErrUndraftBlocked
	}
	repository, _, err := s.Git.RemoteRepository(ctx, s.Project.Repository)
	if err != nil {
		return UndraftDecision{}, err
	}
	if err := s.GitHub.MarkReady(ctx, repository, pr.NodeID); err != nil {
		return UndraftDecision{}, err
	}
	pr.ExpectedState, pr.ObservedState = "ready", "ready"
	pr.Cursor = pullRequestCursor(pr)
	op, err := s.Store.Operation(ctx, runID, "pull_request")
	if err != nil {
		return UndraftDecision{}, err
	}
	pr, _, err = s.Store.UpsertPullRequest(ctx, s.Project.ID, actor, op.ID, pr)
	if err != nil {
		return UndraftDecision{}, err
	}
	details, _ := json.Marshal(map[string]any{"number": pr.Number, "headSha": pr.HeadSHA})
	if _, err := s.Store.UpdateOperation(ctx, s.Project.ID, actor, op, "ready", pr.Cursor, details); err != nil {
		return UndraftDecision{}, err
	}
	decision.PR = pr
	return decision, nil
}

func pullRequestFromObservation(runID string, observation PRObservation, expected string) PullRequest {
	observed := "ready"
	if observation.Draft {
		observed = "draft"
	}
	pr := PullRequest{RunID: runID, Number: observation.Number, NodeID: observation.NodeID, TargetBranch: observation.TargetBranch, HeadBranch: observation.HeadBranch, ExpectedState: expected, ObservedState: observed, HeadSHA: observation.HeadSHA, CIState: observation.CIState, ConflictState: observation.ConflictState, ReviewState: observation.ReviewState}
	pr.Cursor = pullRequestCursor(pr)
	return pr
}

func pullRequestCursor(pr PullRequest) string {
	content, _ := json.Marshal([]any{pr.Number, pr.ObservedState, pr.HeadSHA, pr.CIState, pr.ConflictState, pr.ReviewState})
	hash := sha256.Sum256(content)
	return hex.EncodeToString(hash[:])
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	var result []string
	for _, value := range values {
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result
}

var _ GitHub = GHAdapter{}
var _ = errors.Is
