package nagi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

const schemaVersion = 1

type Store struct {
	db            *sql.DB
	clock         Clock
	writeLockFile *os.File
}

func OpenStore(path string, clock Clock) (*Store, error) {
	if clock == nil {
		clock = SystemClock{}
	}
	dsn := "file:" + url.PathEscape(path) + "?_txlock=immediate&_pragma=foreign_keys(1)"
	writeLockFile, err := os.OpenFile(path+".write.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		writeLockFile.Close()
		return nil, err
	}
	store := &Store{db: db, clock: clock, writeLockFile: writeLockFile}
	release, err := store.acquireWrite()
	if err == nil {
		err = store.migrate(context.Background())
		release()
	}
	if err != nil {
		db.Close()
		writeLockFile.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return errors.Join(s.db.Close(), s.writeLockFile.Close()) }

func (s *Store) acquireWrite() (func(), error) {
	for {
		err := syscall.Flock(int(s.writeLockFile.Fd()), syscall.LOCK_EX)
		if err == nil {
			return func() { _ = syscall.Flock(int(s.writeLockFile.Fd()), syscall.LOCK_UN) }, nil
		}
		if !errors.Is(err, syscall.EINTR) {
			return nil, err
		}
	}
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		return fmt.Errorf("enable WAL: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		return fmt.Errorf("enable foreign keys: %w", err)
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS projects (
			id TEXT PRIMARY KEY, repository TEXT NOT NULL UNIQUE, state_dir TEXT NOT NULL,
			db_path TEXT NOT NULL, config_path TEXT NOT NULL, created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id), title TEXT NOT NULL,
			parent_id TEXT REFERENCES tasks(id), dependency_id TEXT REFERENCES tasks(id),
			integration_lane TEXT NOT NULL CHECK(integration_lane IN ('base','master')),
			base_ref TEXT NOT NULL, state TEXT NOT NULL CHECK(state IN ('ready','claimed','running','completed','failed','cancelled')),
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS runs (
			id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id), task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id),
			state TEXT NOT NULL, base_sha TEXT NOT NULL, worktree_path TEXT NOT NULL UNIQUE,
			branch TEXT NOT NULL UNIQUE, derived_data_path TEXT NOT NULL UNIQUE, runner_session TEXT NOT NULL UNIQUE,
			runner_pid INTEGER NOT NULL DEFAULT 0, runner_status_path TEXT NOT NULL, runner_log_path TEXT NOT NULL,
			final_sha TEXT NOT NULL DEFAULT '', disposition TEXT NOT NULL DEFAULT '', artifacts_saved INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS operations (
			id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id), kind TEXT NOT NULL,
			desired_state TEXT NOT NULL, observed_state TEXT NOT NULL, cursor TEXT NOT NULL DEFAULT '',
			details BLOB, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(run_id, kind)
		)`,
		`CREATE TABLE IF NOT EXISTS operation_steps (
			operation_id TEXT NOT NULL REFERENCES operations(id), name TEXT NOT NULL, state TEXT NOT NULL,
			before_json BLOB, after_json BLOB, updated_at TEXT NOT NULL, PRIMARY KEY(operation_id, name)
		)`,
		`CREATE TABLE IF NOT EXISTS audit_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT, project_id TEXT NOT NULL REFERENCES projects(id), actor TEXT NOT NULL,
			operation_id TEXT, event_type TEXT NOT NULL, occurred_at TEXT NOT NULL, before_json BLOB, after_json BLOB
		)`,
		`CREATE TABLE IF NOT EXISTS qa_runs (
			id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id), candidate_sha TEXT NOT NULL,
			state TEXT NOT NULL, worktree_path TEXT NOT NULL UNIQUE, derived_data_path TEXT NOT NULL UNIQUE,
			selected_xcode TEXT NOT NULL DEFAULT '', packet_json BLOB NOT NULL, artifact_dir TEXT NOT NULL,
			validated_sha TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
			UNIQUE(run_id, candidate_sha)
		)`,
		`CREATE TABLE IF NOT EXISTS qa_criteria (
			qa_id TEXT NOT NULL REFERENCES qa_runs(id), name TEXT NOT NULL, status TEXT NOT NULL,
			observed_json BLOB NOT NULL, reproducibility TEXT NOT NULL, artifacts_json BLOB NOT NULL,
			PRIMARY KEY(qa_id, name)
		)`,
		`CREATE TABLE IF NOT EXISTS pull_requests (
			run_id TEXT PRIMARY KEY REFERENCES runs(id), number INTEGER NOT NULL, node_id TEXT NOT NULL,
			target_branch TEXT NOT NULL, head_branch TEXT NOT NULL, expected_state TEXT NOT NULL,
			observed_state TEXT NOT NULL, head_sha TEXT NOT NULL, ci_state TEXT NOT NULL,
			conflict_state TEXT NOT NULL, review_state TEXT NOT NULL, cursor TEXT NOT NULL, updated_at TEXT NOT NULL
		)`,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES(?, ?)`,
		schemaVersion, formatTime(s.clock.Now())); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Verify(ctx context.Context) (map[string]any, error) {
	result := map[string]any{"schemaVersion": schemaVersion}
	var foreignKeys int
	if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return nil, err
	}
	var journalMode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		return nil, err
	}
	var integrity string
	if err := s.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return nil, err
	}
	result["foreignKeys"] = foreignKeys == 1
	result["journalMode"] = strings.ToLower(journalMode)
	result["integrity"] = integrity
	return result, nil
}

func (s *Store) CreateProject(ctx context.Context, project Project, actor, operationID string) error {
	release, err := s.acquireWrite()
	if err != nil {
		return err
	}
	defer release()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO projects(id, repository, state_dir, db_path, config_path, created_at) VALUES(?,?,?,?,?,?)`,
		project.ID, project.Repository, project.StateDir, project.DBPath, project.ConfigPath, formatTime(project.CreatedAt))
	if err != nil {
		return err
	}
	after, _ := json.Marshal(project)
	if err := insertAudit(ctx, tx, project.ID, actor, operationID, "project.created", nil, after, s.clock.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Project(ctx context.Context, id string) (Project, error) {
	var p Project
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT id, repository, state_dir, db_path, config_path, created_at FROM projects WHERE id=?`, id).
		Scan(&p.ID, &p.Repository, &p.StateDir, &p.DBPath, &p.ConfigPath, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	p.CreatedAt = parseTime(created)
	return p, err
}

func (s *Store) AddTask(ctx context.Context, task Task, actor, operationID string) error {
	release, err := s.acquireWrite()
	if err != nil {
		return err
	}
	defer release()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(id, project_id, title, parent_id, dependency_id, integration_lane, base_ref, state, created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		task.ID, task.ProjectID, task.Title, nullable(task.ParentID), nullable(task.DependencyID), task.IntegrationLane, task.BaseRef, task.State, formatTime(task.CreatedAt)); err != nil {
		return err
	}
	after, _ := json.Marshal(task)
	if err := insertAudit(ctx, tx, task.ProjectID, actor, operationID, "task.created", nil, after, s.clock.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Task(ctx context.Context, id string) (Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, project_id, title, COALESCE(parent_id,''), COALESCE(dependency_id,''), integration_lane, base_ref, state, created_at FROM tasks WHERE id=?`, id)
	return scanTask(row)
}

func (s *Store) Tasks(ctx context.Context, projectID string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, project_id, title, COALESCE(parent_id,''), COALESCE(dependency_id,''), integration_lane, base_ref, state, created_at FROM tasks WHERE project_id=? ORDER BY created_at,id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

type Claim struct {
	Run       Run
	Operation Operation
}

func (s *Store) ClaimTask(ctx context.Context, task Task, run Run, operation Operation, actor string) (Claim, error) {
	release, err := s.acquireWrite()
	if err != nil {
		return Claim{}, err
	}
	defer release()
	for {
		claim, claimErr := s.claimOnce(ctx, task, run, operation, actor)
		if !isSQLiteBusy(claimErr) {
			return claim, claimErr
		}
		select {
		case <-ctx.Done():
			return Claim{}, ctx.Err()
		default:
			runtime.Gosched()
		}
	}
}

func (s *Store) claimOnce(ctx context.Context, task Task, run Run, operation Operation, actor string) (Claim, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Claim{}, err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id=?`, task.ID).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Claim{}, ErrNotFound
		}
		return Claim{}, err
	}
	if state != "ready" {
		if state == "claimed" || state == "running" || state == "completed" {
			return Claim{}, ErrAlreadyClaimed
		}
		return Claim{}, ErrNotReady
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs(id, project_id, task_id, state, base_sha, worktree_path, branch, derived_data_path, runner_session, runner_pid, runner_status_path, runner_log_path, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.ID, run.ProjectID, run.TaskID, run.State, run.BaseSHA, run.WorktreePath, run.Branch, run.DerivedDataPath, run.RunnerSession, 0, run.RunnerStatusPath, run.RunnerLogPath, formatTime(run.CreatedAt), formatTime(run.UpdatedAt)); err != nil {
		if isUniqueConstraint(err) {
			return Claim{}, ErrAlreadyClaimed
		}
		return Claim{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO operations(id, run_id, kind, desired_state, observed_state, cursor, details, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		operation.ID, operation.RunID, operation.Kind, operation.DesiredState, operation.ObservedState, operation.Cursor, operation.Details, formatTime(operation.CreatedAt), formatTime(operation.UpdatedAt)); err != nil {
		return Claim{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state='claimed' WHERE id=? AND state='ready'`, task.ID); err != nil {
		return Claim{}, err
	}
	after, _ := json.Marshal(run)
	if err := insertAudit(ctx, tx, run.ProjectID, actor, operation.ID, "run.claimed", nil, after, s.clock.Now()); err != nil {
		return Claim{}, err
	}
	if err := tx.Commit(); err != nil {
		return Claim{}, err
	}
	return Claim{Run: run, Operation: operation}, nil
}

func (s *Store) Run(ctx context.Context, id string) (Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, runSelect+` WHERE id=?`, id))
}

func (s *Store) RunForTask(ctx context.Context, taskID string) (Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, runSelect+` WHERE task_id=?`, taskID))
}

func (s *Store) Runs(ctx context.Context, projectID string) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, runSelect+` WHERE project_id=? ORDER BY created_at,id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) Operations(ctx context.Context, projectID string) ([]Operation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT o.id,o.run_id,o.kind,o.desired_state,o.observed_state,o.cursor,COALESCE(o.details,X''),o.created_at,o.updated_at FROM operations o JOIN runs r ON r.id=o.run_id WHERE r.project_id=? ORDER BY o.created_at,o.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var operations []Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		operations = append(operations, op)
	}
	return operations, rows.Err()
}

func (s *Store) Operation(ctx context.Context, runID, kind string) (Operation, error) {
	return scanOperation(s.db.QueryRowContext(ctx, `SELECT id,run_id,kind,desired_state,observed_state,cursor,COALESCE(details,X''),created_at,updated_at FROM operations WHERE run_id=? AND kind=?`, runID, kind))
}

func (s *Store) EnsureOperation(ctx context.Context, op Operation) (Operation, error) {
	release, err := s.acquireWrite()
	if err != nil {
		return Operation{}, err
	}
	defer release()
	_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO operations(id,run_id,kind,desired_state,observed_state,cursor,details,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		op.ID, op.RunID, op.Kind, op.DesiredState, op.ObservedState, op.Cursor, op.Details, formatTime(op.CreatedAt), formatTime(op.UpdatedAt))
	if err != nil {
		return Operation{}, err
	}
	return s.Operation(ctx, op.RunID, op.Kind)
}

func (s *Store) UpdateOperation(ctx context.Context, projectID, actor string, before Operation, observed, cursor string, details json.RawMessage) (Operation, error) {
	release, err := s.acquireWrite()
	if err != nil {
		return Operation{}, err
	}
	defer release()
	after := before
	after.ObservedState = observed
	after.Cursor = cursor
	after.Details = details
	after.UpdatedAt = s.clock.Now()
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	for {
		err = func() error {
			tx, txErr := s.db.BeginTx(ctx, nil)
			if txErr != nil {
				return txErr
			}
			defer tx.Rollback()
			if _, txErr = tx.ExecContext(ctx, `UPDATE operations SET observed_state=?,cursor=?,details=?,updated_at=? WHERE id=?`, observed, cursor, details, formatTime(after.UpdatedAt), before.ID); txErr != nil {
				return txErr
			}
			if txErr = insertAudit(ctx, tx, projectID, actor, before.ID, "operation.updated", beforeJSON, afterJSON, s.clock.Now()); txErr != nil {
				return txErr
			}
			return tx.Commit()
		}()
		if !isSQLiteBusy(err) {
			break
		}
		select {
		case <-ctx.Done():
			return Operation{}, ctx.Err()
		default:
			runtime.Gosched()
		}
	}
	if err != nil {
		return Operation{}, err
	}
	return after, nil
}

func (s *Store) Step(ctx context.Context, operationID, name string) (OperationStep, error) {
	var step OperationStep
	var before, after []byte
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT operation_id,name,state,COALESCE(before_json,X''),COALESCE(after_json,X''),updated_at FROM operation_steps WHERE operation_id=? AND name=?`, operationID, name).
		Scan(&step.OperationID, &step.Name, &step.State, &before, &after, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return step, ErrNotFound
	}
	step.Before, step.After, step.UpdatedAt = before, after, parseTime(updated)
	return step, err
}

func (s *Store) CompleteStep(ctx context.Context, projectID, actor string, op Operation, name string, before, after any) error {
	release, err := s.acquireWrite()
	if err != nil {
		return err
	}
	defer release()
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	for {
		err = func() error {
			tx, txErr := s.db.BeginTx(ctx, nil)
			if txErr != nil {
				return txErr
			}
			defer tx.Rollback()
			if _, txErr = tx.ExecContext(ctx, `INSERT INTO operation_steps(operation_id,name,state,before_json,after_json,updated_at) VALUES(?,?,'completed',?,?,?) ON CONFLICT(operation_id,name) DO UPDATE SET state='completed',before_json=excluded.before_json,after_json=excluded.after_json,updated_at=excluded.updated_at`, op.ID, name, beforeJSON, afterJSON, formatTime(s.clock.Now())); txErr != nil {
				return txErr
			}
			if txErr = insertAudit(ctx, tx, projectID, actor, op.ID, "operation.step.completed", beforeJSON, afterJSON, s.clock.Now()); txErr != nil {
				return txErr
			}
			return tx.Commit()
		}()
		if !isSQLiteBusy(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			runtime.Gosched()
		}
	}
}

func (s *Store) UpdateRunProvisioned(ctx context.Context, run Run, process RunnerProcess, actor, operationID string) (Run, error) {
	release, err := s.acquireWrite()
	if err != nil {
		return Run{}, err
	}
	defer release()
	before := run
	run.RunnerPID = process.PID
	run.RunnerSession = process.SessionID
	run.RunnerStatusPath = process.StatusPath
	run.RunnerLogPath = process.LogPath
	run.State = "running"
	run.UpdatedAt = s.clock.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET runner_pid=?,runner_session=?,runner_status_path=?,runner_log_path=?,state=?,updated_at=? WHERE id=?`, process.PID, process.SessionID, process.StatusPath, process.LogPath, run.State, formatTime(run.UpdatedAt), run.ID); err != nil {
		return Run{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state='running' WHERE id=?`, run.TaskID); err != nil {
		return Run{}, err
	}
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(run)
	if err := insertAudit(ctx, tx, run.ProjectID, actor, operationID, "run.running", beforeJSON, afterJSON, s.clock.Now()); err != nil {
		return Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return Run{}, err
	}
	return run, nil
}

func (s *Store) UpdateRunState(ctx context.Context, run Run, state, finalSHA, disposition string, artifactsSaved bool, actor, operationID string) (Run, error) {
	release, err := s.acquireWrite()
	if err != nil {
		return Run{}, err
	}
	defer release()
	before := run
	run.State = state
	if finalSHA != "" {
		run.FinalSHA = finalSHA
	}
	if disposition != "" {
		run.Disposition = disposition
	}
	run.ArtifactsSaved = artifactsSaved || run.ArtifactsSaved
	run.UpdatedAt = s.clock.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET state=?,final_sha=?,disposition=?,artifacts_saved=?,updated_at=? WHERE id=?`, run.State, run.FinalSHA, run.Disposition, boolInt(run.ArtifactsSaved), formatTime(run.UpdatedAt), run.ID); err != nil {
		return Run{}, err
	}
	if state == "completed" || state == "failed" || state == "cancelled" {
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state=? WHERE id=?`, state, run.TaskID); err != nil {
			return Run{}, err
		}
	}
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(run)
	if err := insertAudit(ctx, tx, run.ProjectID, actor, operationID, "run.state.changed", beforeJSON, afterJSON, s.clock.Now()); err != nil {
		return Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return Run{}, err
	}
	return run, nil
}

func (s *Store) Events(ctx context.Context, projectID string) ([]AuditEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,project_id,actor,COALESCE(operation_id,''),event_type,occurred_at,COALESCE(before_json,X''),COALESCE(after_json,X'') FROM audit_events WHERE project_id=? ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []AuditEvent
	for rows.Next() {
		var event AuditEvent
		var occurred string
		if err := rows.Scan(&event.ID, &event.ProjectID, &event.Actor, &event.OperationID, &event.Type, &occurred, &event.Before, &event.After); err != nil {
			return nil, err
		}
		event.OccurredAt = parseTime(occurred)
		events = append(events, event)
	}
	return events, rows.Err()
}

const runSelect = `SELECT id,project_id,task_id,state,base_sha,worktree_path,branch,derived_data_path,runner_session,runner_pid,runner_status_path,runner_log_path,final_sha,disposition,artifacts_saved,created_at,updated_at FROM runs`

type scanner interface{ Scan(...any) error }

func scanTask(row scanner) (Task, error) {
	var task Task
	var created string
	err := row.Scan(&task.ID, &task.ProjectID, &task.Title, &task.ParentID, &task.DependencyID, &task.IntegrationLane, &task.BaseRef, &task.State, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return task, ErrNotFound
	}
	task.CreatedAt = parseTime(created)
	return task, err
}

func scanRun(row scanner) (Run, error) {
	var run Run
	var artifacts int
	var created, updated string
	err := row.Scan(&run.ID, &run.ProjectID, &run.TaskID, &run.State, &run.BaseSHA, &run.WorktreePath, &run.Branch, &run.DerivedDataPath, &run.RunnerSession, &run.RunnerPID, &run.RunnerStatusPath, &run.RunnerLogPath, &run.FinalSHA, &run.Disposition, &artifacts, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return run, ErrNotFound
	}
	run.ArtifactsSaved = artifacts == 1
	run.CreatedAt, run.UpdatedAt = parseTime(created), parseTime(updated)
	return run, err
}

func scanOperation(row scanner) (Operation, error) {
	var op Operation
	var details []byte
	var created, updated string
	err := row.Scan(&op.ID, &op.RunID, &op.Kind, &op.DesiredState, &op.ObservedState, &op.Cursor, &details, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return op, ErrNotFound
	}
	op.CreatedAt, op.UpdatedAt = parseTime(created), parseTime(updated)
	op.Details = details
	return op, err
}

func insertAudit(ctx context.Context, tx *sql.Tx, projectID, actor, operationID, eventType string, before, after []byte, at time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_events(project_id,actor,operation_id,event_type,occurred_at,before_json,after_json) VALUES(?,?,?,?,?,?,?)`,
		projectID, actor, nullable(operationID), eventType, formatTime(at), before, after)
	return err
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
func parseTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
func isUniqueConstraint(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "constraint failed"))
}
func isSQLiteBusy(err error) bool {
	return err != nil && (strings.Contains(strings.ToLower(err.Error()), "database is locked") || strings.Contains(strings.ToLower(err.Error()), "busy"))
}
