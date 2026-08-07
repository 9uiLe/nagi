package nagi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

func (s *Store) EnsureQARun(ctx context.Context, qa QARun) (QARun, error) {
	release, err := s.acquireWrite()
	if err != nil {
		return QARun{}, err
	}
	defer release()
	_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO qa_runs(id,run_id,candidate_sha,state,worktree_path,derived_data_path,selected_xcode,packet_json,artifact_dir,validated_sha,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		qa.ID, qa.RunID, qa.CandidateSHA, qa.State, qa.WorktreePath, qa.DerivedDataPath, qa.SelectedXcode, qa.Packet, qa.ArtifactDir, qa.ValidatedSHA, formatTime(qa.CreatedAt), formatTime(qa.UpdatedAt))
	if err != nil {
		return QARun{}, err
	}
	return s.QARunByCandidate(ctx, qa.RunID, qa.CandidateSHA)
}

func (s *Store) QARunByCandidate(ctx context.Context, runID, candidate string) (QARun, error) {
	return scanQARun(s.db.QueryRowContext(ctx, `SELECT id,run_id,candidate_sha,state,worktree_path,derived_data_path,selected_xcode,packet_json,artifact_dir,validated_sha,created_at,updated_at FROM qa_runs WHERE run_id=? AND candidate_sha=?`, runID, candidate))
}

func (s *Store) LatestQARun(ctx context.Context, runID string) (QARun, error) {
	return scanQARun(s.db.QueryRowContext(ctx, `SELECT id,run_id,candidate_sha,state,worktree_path,derived_data_path,selected_xcode,packet_json,artifact_dir,validated_sha,created_at,updated_at FROM qa_runs WHERE run_id=? ORDER BY updated_at DESC LIMIT 1`, runID))
}

func (s *Store) UpdateQARun(ctx context.Context, qa QARun, state, selectedXcode, validatedSHA, projectID, actor, operationID string) (QARun, error) {
	release, err := s.acquireWrite()
	if err != nil {
		return QARun{}, err
	}
	defer release()
	before := qa
	qa.State = state
	if selectedXcode != "" {
		qa.SelectedXcode = selectedXcode
	}
	qa.ValidatedSHA = validatedSHA
	qa.UpdatedAt = s.clock.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return QARun{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE qa_runs SET state=?,selected_xcode=?,validated_sha=?,updated_at=? WHERE id=?`, qa.State, qa.SelectedXcode, qa.ValidatedSHA, formatTime(qa.UpdatedAt), qa.ID); err != nil {
		return QARun{}, err
	}
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(qa)
	if err := insertAudit(ctx, tx, projectID, actor, operationID, "qa.state.changed", beforeJSON, afterJSON, s.clock.Now()); err != nil {
		return QARun{}, err
	}
	if err := tx.Commit(); err != nil {
		return QARun{}, err
	}
	return qa, nil
}

func (s *Store) SaveQACriterion(ctx context.Context, result QACriterionResult) error {
	release, err := s.acquireWrite()
	if err != nil {
		return err
	}
	defer release()
	artifacts, _ := json.Marshal(result.Artifacts)
	_, err = s.db.ExecContext(ctx, `INSERT INTO qa_criteria(qa_id,name,status,observed_json,reproducibility,artifacts_json) VALUES(?,?,?,?,?,?) ON CONFLICT(qa_id,name) DO UPDATE SET status=excluded.status,observed_json=excluded.observed_json,reproducibility=excluded.reproducibility,artifacts_json=excluded.artifacts_json`,
		result.QAID, result.Name, result.Status, result.Observed, result.Reproducibility, artifacts)
	return err
}

func (s *Store) QACriterion(ctx context.Context, qaID, name string) (QACriterionResult, error) {
	var result QACriterionResult
	var artifacts []byte
	err := s.db.QueryRowContext(ctx, `SELECT qa_id,name,status,observed_json,reproducibility,artifacts_json FROM qa_criteria WHERE qa_id=? AND name=?`, qaID, name).
		Scan(&result.QAID, &result.Name, &result.Status, &result.Observed, &result.Reproducibility, &artifacts)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrNotFound
	}
	if err == nil {
		err = json.Unmarshal(artifacts, &result.Artifacts)
	}
	return result, err
}

func (s *Store) QACriteria(ctx context.Context, qaID string) ([]QACriterionResult, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT qa_id,name,status,observed_json,reproducibility,artifacts_json FROM qa_criteria WHERE qa_id=? ORDER BY name`, qaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []QACriterionResult
	for rows.Next() {
		var result QACriterionResult
		var artifacts []byte
		if err := rows.Scan(&result.QAID, &result.Name, &result.Status, &result.Observed, &result.Reproducibility, &artifacts); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(artifacts, &result.Artifacts); err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, rows.Err()
}

func scanQARun(row scanner) (QARun, error) {
	var qa QARun
	var created, updated string
	err := row.Scan(&qa.ID, &qa.RunID, &qa.CandidateSHA, &qa.State, &qa.WorktreePath, &qa.DerivedDataPath, &qa.SelectedXcode, &qa.Packet, &qa.ArtifactDir, &qa.ValidatedSHA, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return qa, ErrNotFound
	}
	qa.CreatedAt, qa.UpdatedAt = parseTime(created), parseTime(updated)
	return qa, err
}

func (s *Store) PullRequest(ctx context.Context, runID string) (PullRequest, error) {
	var pr PullRequest
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT run_id,number,node_id,target_branch,head_branch,expected_state,observed_state,head_sha,ci_state,conflict_state,review_state,cursor,updated_at FROM pull_requests WHERE run_id=?`, runID).
		Scan(&pr.RunID, &pr.Number, &pr.NodeID, &pr.TargetBranch, &pr.HeadBranch, &pr.ExpectedState, &pr.ObservedState, &pr.HeadSHA, &pr.CIState, &pr.ConflictState, &pr.ReviewState, &pr.Cursor, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return pr, ErrNotFound
	}
	pr.UpdatedAt = parseTime(updated)
	return pr, err
}

func (s *Store) UpsertPullRequest(ctx context.Context, projectID, actor, operationID string, pr PullRequest) (PullRequest, bool, error) {
	release, err := s.acquireWrite()
	if err != nil {
		return PullRequest{}, false, err
	}
	defer release()
	before, err := s.PullRequest(ctx, pr.RunID)
	exists := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return PullRequest{}, false, err
	}
	if exists && before.Cursor == pr.Cursor && before.ExpectedState == pr.ExpectedState {
		return before, false, nil
	}
	pr.UpdatedAt = s.clock.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PullRequest{}, false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO pull_requests(run_id,number,node_id,target_branch,head_branch,expected_state,observed_state,head_sha,ci_state,conflict_state,review_state,cursor,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(run_id) DO UPDATE SET number=excluded.number,node_id=excluded.node_id,target_branch=excluded.target_branch,head_branch=excluded.head_branch,expected_state=excluded.expected_state,observed_state=excluded.observed_state,head_sha=excluded.head_sha,ci_state=excluded.ci_state,conflict_state=excluded.conflict_state,review_state=excluded.review_state,cursor=excluded.cursor,updated_at=excluded.updated_at`,
		pr.RunID, pr.Number, pr.NodeID, pr.TargetBranch, pr.HeadBranch, pr.ExpectedState, pr.ObservedState, pr.HeadSHA, pr.CIState, pr.ConflictState, pr.ReviewState, pr.Cursor, formatTime(pr.UpdatedAt)); err != nil {
		return PullRequest{}, false, err
	}
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(pr)
	if err := insertAudit(ctx, tx, projectID, actor, operationID, "pull_request.changed", beforeJSON, afterJSON, s.clock.Now()); err != nil {
		return PullRequest{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return PullRequest{}, false, err
	}
	return pr, true, nil
}
