package nagi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestProjectDatabaseAndTaskAxes(t *testing.T) {
	repository := newTestRepository(t)
	service := newTestService(t, repository)
	verification, err := service.Store.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if verification["foreignKeys"] != true || verification["journalMode"] != "wal" || verification["integrity"] != "ok" {
		t.Fatalf("%v", verification)
	}
	if filepath.Clean(service.Project.DBPath) == filepath.Join(repository, "nagi.sqlite") || filepathHasPrefix(service.Project.DBPath, repository) {
		t.Fatalf("database is inside repository: %s", service.Project.DBPath)
	}
	parent := addReadyTask(t, service, "parent")
	baseTask, err := service.AddTask(context.Background(), Task{ID: "base-child", Title: "base child", ParentID: parent.ID, DependencyID: parent.ID, IntegrationLane: "base", BaseRef: "master"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	masterTask, err := service.AddTask(context.Background(), Task{ID: "master-child", Title: "master child", ParentID: parent.ID, IntegrationLane: "master", BaseRef: "master"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if baseTask.ParentID != masterTask.ParentID || baseTask.DependencyID == masterTask.DependencyID || baseTask.IntegrationLane == masterTask.IntegrationLane {
		t.Fatalf("axes collapsed: %+v %+v", baseTask, masterTask)
	}
}

func TestConcurrentClaimCreatesOneRunAndStableLoser(t *testing.T) {
	repository := newTestRepository(t)
	service := newTestService(t, repository)
	addReadyTask(t, service, "claim-me")
	stateRoot := filepath.Dir(filepath.Dir(service.Project.StateDir))
	second, err := OpenService(context.Background(), service.Project.ID, stateRoot, service.Clock, RandomIDs{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.Runner = newFakeRunner()
	services := []*Service{service, second}
	var wg sync.WaitGroup
	errorsSeen := make(chan error, len(services))
	for _, candidate := range services {
		wg.Add(1)
		go func(candidate *Service) {
			defer wg.Done()
			_, err := candidate.StartTask(context.Background(), "claim-me", StartOptions{Actor: "concurrent"})
			errorsSeen <- err
		}(candidate)
	}
	wg.Wait()
	close(errorsSeen)
	successes, losers := 0, 0
	for err := range errorsSeen {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrAlreadyClaimed) {
			losers++
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != 1 || losers != 1 {
		t.Fatalf("successes=%d losers=%d", successes, losers)
	}
	runs, err := service.Store.Runs(context.Background(), service.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	operations, err := service.Store.Operations(context.Background(), service.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || len(operations) != 1 {
		t.Fatalf("runs=%d operations=%d", len(runs), len(operations))
	}
}

func TestFaultAfterDesiredStateResumesSameOperationWithoutDuplicateRunner(t *testing.T) {
	repository := newTestRepository(t)
	service := newTestService(t, repository)
	addReadyTask(t, service, "restartable")
	run, err := service.StartTask(context.Background(), "restartable", StartOptions{Actor: "first", FaultAfter: "plan"})
	if !errors.Is(err, ErrInjectedFault) {
		t.Fatalf("got %v", err)
	}
	if _, statErr := os.Stat(run.WorktreePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("side effect occurred before resume: %v", statErr)
	}
	stateRoot := filepath.Dir(filepath.Dir(service.Project.StateDir))
	resumer, err := OpenService(context.Background(), service.Project.ID, stateRoot, service.Clock, RandomIDs{})
	if err != nil {
		t.Fatal(err)
	}
	defer resumer.Close()
	runner := newFakeRunner()
	resumer.Runner = runner
	resumed, err := resumer.ResumeProvisioning(context.Background(), "resumer")
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 1 || resumed[0].ID != run.ID || resumed[0].State != "running" {
		t.Fatalf("%+v", resumed)
	}
	if runner.starts[run.ID] != 1 {
		t.Fatalf("runner starts=%d", runner.starts[run.ID])
	}
	operations, err := resumer.Store.Operations(context.Background(), service.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].ObservedState != "running" {
		t.Fatalf("%+v", operations)
	}
}

func TestRegisteredSeedSecretNeverEntersDatabaseEventsOrRunnerLog(t *testing.T) {
	repository := newTestRepository(t)
	mustWrite(t, filepath.Join(repository, ".nagi.json"), []byte(`{"argv":["/usr/bin/true"],"seedFiles":["seed.txt"]}`))
	runCommand(t, repository, "git", "add", ".nagi.json")
	runCommand(t, repository, "git", "commit", "-m", "trusted seed config")
	service := newTestService(t, repository)
	secret := []byte("nagi-test-secret-content")
	source := filepath.Join(t.TempDir(), "secret.txt")
	mustWrite(t, source, secret)
	if _, err := service.RegisterSeed(source, "seed.txt"); err != nil {
		t.Fatal(err)
	}
	addReadyTask(t, service, "seeded")
	run, err := service.StartTask(context.Background(), "seeded", StartOptions{Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(filepath.Join(run.WorktreePath, "seed.txt"))
	if err != nil || !bytes.Equal(copied, secret) {
		t.Fatalf("copied=%q err=%v", copied, err)
	}
	for _, path := range []string{service.Project.DBPath, service.Project.DBPath + "-wal", run.RunnerLogPath} {
		content, readErr := os.ReadFile(path)
		if readErr != nil && errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		if bytes.Contains(content, secret) {
			t.Fatalf("secret persisted in %s", path)
		}
	}
	events, err := service.Store.Events(context.Background(), service.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(events)
	if bytes.Contains(encoded, secret) {
		t.Fatal("secret persisted in audit event")
	}
}

func TestTwoReadyTasksProvisionIsolatedMappings(t *testing.T) {
	repository := newTestRepository(t)
	service := newTestService(t, repository)
	addReadyTask(t, service, "alpha")
	addReadyTask(t, service, "beta")
	var wg sync.WaitGroup
	runs := make(chan Run, 2)
	failures := make(chan error, 2)
	for _, id := range []string{"alpha", "beta"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			run, err := service.StartTask(context.Background(), id, StartOptions{Actor: "parallel"})
			if err != nil {
				failures <- err
				return
			}
			runs <- run
		}(id)
	}
	wg.Wait()
	close(runs)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	var got []Run
	for run := range runs {
		got = append(got, run)
	}
	if len(got) != 2 {
		t.Fatalf("runs=%d", len(got))
	}
	if got[0].WorktreePath == got[1].WorktreePath || got[0].Branch == got[1].Branch || got[0].DerivedDataPath == got[1].DerivedDataPath || got[0].RunnerSession == got[1].RunnerSession {
		t.Fatalf("not isolated: %+v", got)
	}
	for _, run := range got {
		sha, err := service.Git.RevParse(context.Background(), run.WorktreePath, "HEAD")
		if err != nil || sha != run.BaseSHA {
			t.Fatalf("sha=%q err=%v run=%+v", sha, err, run)
		}
	}
	snapshot, err := service.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Runs) != 2 || len(snapshot.Tasks) != 2 {
		t.Fatalf("%+v", snapshot)
	}
}

func TestReconcileClassifiesLostUnmanagedMismatchAndDirtyWithoutDeleting(t *testing.T) {
	repository := newTestRepository(t)
	service := newTestService(t, repository)
	for _, id := range []string{"lost", "mismatch", "dirty"} {
		addReadyTask(t, service, id)
	}
	lost, err := service.StartTask(context.Background(), "lost", StartOptions{Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	mismatch, err := service.StartTask(context.Background(), "mismatch", StartOptions{Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	dirty, err := service.StartTask(context.Background(), "dirty", StartOptions{Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	runCommand(t, repository, "git", "worktree", "unlock", lost.WorktreePath)
	runCommand(t, repository, "git", "worktree", "remove", lost.WorktreePath)
	mustWrite(t, filepath.Join(mismatch.WorktreePath, "change.txt"), []byte("committed\n"))
	runCommand(t, mismatch.WorktreePath, "git", "add", ".")
	runCommand(t, mismatch.WorktreePath, "git", "commit", "-m", "move head")
	mustWrite(t, filepath.Join(dirty.WorktreePath, "dirty.txt"), []byte("dirty\n"))
	unmanaged := filepath.Join(t.TempDir(), "unmanaged")
	runCommand(t, repository, "git", "worktree", "add", "--detach", unmanaged, "master")
	findings, err := service.ReconcileWorktrees(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, finding := range findings {
		kinds[finding.Kind] = true
		if !finding.RequiresUser {
			t.Fatalf("finding is auto-actionable: %+v", finding)
		}
	}
	for _, kind := range []string{"lost_worktree", "unmanaged", "state_mismatch", "dirty"} {
		if !kinds[kind] {
			t.Fatalf("missing %s in %+v", kind, findings)
		}
	}
	if _, err := os.Stat(unmanaged); err != nil {
		t.Fatalf("unmanaged worktree was deleted: %v", err)
	}
	if _, err := os.Stat(dirty.WorktreePath); err != nil {
		t.Fatalf("dirty worktree was deleted: %v", err)
	}
}

func TestCleanupResumesStepsAndPreservesDiscardedBranch(t *testing.T) {
	repository := newTestRepository(t)
	service := newTestService(t, repository)
	addReadyTask(t, service, "cleanup")
	run, err := service.StartTask(context.Background(), "cleanup", StartOptions{Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	run, err = service.CompleteRun(context.Background(), run.ID, "discarded", "test")
	if err != nil {
		t.Fatal(err)
	}
	recorder := &recordingExecutor{delegate: OSExecutor{}}
	service.Git = GitAdapter{Exec: recorder}
	result, err := service.CleanupRun(context.Background(), run.ID, CleanupOptions{Actor: "test", FaultAfter: "worktree_removed"})
	if !errors.Is(err, ErrInjectedFault) {
		t.Fatalf("result=%v err=%v", result, err)
	}
	if _, err := os.Stat(run.WorktreePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree remains: %v", err)
	}
	result, err = service.CleanupRun(context.Background(), run.ID, CleanupOptions{Actor: "resume"})
	if err != nil {
		t.Fatalf("result=%v err=%v", result, err)
	}
	if _, err := os.Stat(run.DerivedDataPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("derived data remains: %v", err)
	}
	if _, err := service.Git.RevParse(context.Background(), repository, run.Branch); err != nil {
		t.Fatalf("discarded unmerged branch was removed: %v", err)
	}
	for _, argv := range recorder.calls {
		for _, arg := range argv {
			if arg == "--force" || arg == "-D" {
				t.Fatalf("destructive git argv used: %v", argv)
			}
		}
	}
	op, err := service.Store.Operation(context.Background(), run.ID, "cleanup")
	if err != nil || op.ObservedState != "cleaned" {
		t.Fatalf("op=%+v err=%v", op, err)
	}
}

func TestCleanupRefusesDirtyWorktree(t *testing.T) {
	repository := newTestRepository(t)
	service := newTestService(t, repository)
	addReadyTask(t, service, "dirty-cleanup")
	run, err := service.StartTask(context.Background(), "dirty-cleanup", StartOptions{Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	run, err = service.CompleteRun(context.Background(), run.ID, "discarded", "test")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(run.WorktreePath, "uncommitted.txt"), []byte("preserve me"))
	result, err := service.CleanupRun(context.Background(), run.ID, CleanupOptions{Actor: "test"})
	if !errors.Is(err, ErrCleanupBlocked) {
		t.Fatalf("result=%v err=%v", result, err)
	}
	if _, statErr := os.Stat(filepath.Join(run.WorktreePath, "uncommitted.txt")); statErr != nil {
		t.Fatalf("dirty content removed: %v", statErr)
	}
}

func TestEveryRecordedStateChangeCarriesActorOperationTimeAndBeforeAfter(t *testing.T) {
	repository := newTestRepository(t)
	service := newTestService(t, repository)
	addReadyTask(t, service, "audited")
	run, err := service.StartTask(context.Background(), "audited", StartOptions{Actor: "runner-actor"})
	if err != nil {
		t.Fatal(err)
	}
	run, err = service.CompleteRun(context.Background(), run.ID, "discarded", "completion-actor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CleanupRun(context.Background(), run.ID, CleanupOptions{Actor: "cleanup-actor"}); err != nil {
		t.Fatal(err)
	}
	events, err := service.Store.Events(context.Background(), service.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no audit events")
	}
	for _, event := range events {
		if event.Actor == "" || event.OperationID == "" || event.OccurredAt.IsZero() {
			t.Fatalf("missing audit identity: %+v", event)
		}
		if len(event.After) == 0 || !json.Valid(event.After) {
			t.Fatalf("missing after state: %+v", event)
		}
		if len(event.Before) > 0 && !json.Valid(event.Before) {
			t.Fatalf("invalid before state: %+v", event)
		}
	}
}

func filepathHasPrefix(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !filepath.IsAbs(relative) && relative[:min(len(relative), 3)] != "../"
}
