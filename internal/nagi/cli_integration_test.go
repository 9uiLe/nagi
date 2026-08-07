package nagi

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestCLITaskAddReportsValidationErrorsWithoutDetails(t *testing.T) {
	repository := newTestRepository(t)
	stateRoot := filepath.Join(t.TempDir(), "state")
	_, filename, _, _ := runtime.Caller(0)
	moduleRoot, err := filepath.Abs(filepath.Join(filepath.Dir(filename), "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "nagi")
	build := exec.Command("go", "build", "-o", binary, "./cmd/nagi")
	build.Dir = moduleRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	initResult, exit := runCLI(binary, "init", "--repo", repository, "--state-root", stateRoot)
	if exit != 0 {
		t.Fatalf("init exit=%d output=%s", exit, initResult)
	}
	projectID := nestedString(t, initResult, "result", "project", "projectId")

	tests := []struct {
		name    string
		message string
		args    []string
	}{
		{name: "invalid lane", message: "integration lane must be base or master", args: []string{"--id", "invalid-lane", "--title", "invalid lane", "--lane", "main", "--base", "main"}},
		{name: "missing title", message: "task id and title are required", args: []string{"--id", "missing-title", "--lane", "master", "--base", "master"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"task", "add", "--project", projectID, "--state-root", stateRoot}
			result, exit := runCLI(binary, append(args, test.args...)...)
			payload := decodeCLI(t, result)
			if exit != 2 || payload["reason"] != "invalid_arguments" {
				t.Fatalf("exit=%d output=%s", exit, result)
			}
			if payload["error"] != test.message {
				t.Fatalf("error=%q, want %q", payload["error"], test.message)
			}
			if _, ok := payload["details"]; ok {
				t.Fatalf("validation failure included details: %s", result)
			}
		})
	}
}

func TestMultipleCLIProcessesClaimOnceAndResumePersistedOperation(t *testing.T) {
	repository := newTestRepository(t)
	stateRoot := filepath.Join(t.TempDir(), "state")
	_, filename, _, _ := runtime.Caller(0)
	moduleRoot, err := filepath.Abs(filepath.Join(filepath.Dir(filename), "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "nagi")
	build := exec.Command("go", "build", "-o", binary, "./cmd/nagi")
	build.Dir = moduleRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	initResult, exit := runCLI(binary, "init", "--repo", repository, "--state-root", stateRoot)
	if exit != 0 {
		t.Fatalf("init exit=%d output=%s", exit, initResult)
	}
	projectID := nestedString(t, initResult, "result", "project", "projectId")
	addResult, exit := runCLI(binary, "task", "add", "--project", projectID, "--state-root", stateRoot, "--id", "process-race", "--title", "process race", "--lane", "master", "--base", "master")
	if exit != 0 {
		t.Fatalf("add exit=%d output=%s", exit, addResult)
	}
	type outcome struct {
		output []byte
		exit   int
	}
	outcomes := make(chan outcome, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			output, exit := runCLI(binary, "task", "start", "--project", projectID, "--state-root", stateRoot, "--task", "process-race", "--fault-after", "plan")
			outcomes <- outcome{output: output, exit: exit}
		}()
	}
	wg.Wait()
	close(outcomes)
	winner, loser := 0, 0
	for result := range outcomes {
		payload := decodeCLI(t, result.output)
		switch result.exit {
		case 1:
			if payload["reason"] != "injected_fault" {
				t.Fatalf("winner output=%s", result.output)
			}
			winner++
		case 10:
			if payload["reason"] != "already_claimed" {
				t.Fatalf("loser output=%s", result.output)
			}
			loser++
		default:
			t.Fatalf("unexpected exit=%d output=%s", result.exit, result.output)
		}
	}
	if winner != 1 || loser != 1 {
		t.Fatalf("winner=%d loser=%d", winner, loser)
	}
	resumeResult, exit := runCLI(binary, "resume", "--project", projectID, "--state-root", stateRoot)
	if exit != 0 {
		t.Fatalf("resume exit=%d output=%s", exit, resumeResult)
	}
	snapshotResult, exit := runCLI(binary, "snapshot", "--project", projectID, "--state-root", stateRoot)
	if exit != 0 {
		t.Fatalf("snapshot exit=%d output=%s", exit, snapshotResult)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(snapshotResult, &snapshot); err != nil {
		t.Fatal(err)
	}
	result := snapshot["result"].(map[string]any)
	if len(result["runs"].([]any)) != 1 || len(result["operations"].([]any)) != 1 {
		t.Fatalf("snapshot=%s", snapshotResult)
	}
	operation := result["operations"].([]any)[0].(map[string]any)
	if operation["observedState"] != "running" {
		t.Fatalf("operation=%v", operation)
	}
	firstRun := result["runs"].([]any)[0].(map[string]any)
	eventuallyReadStatus(t, firstRun["runnerStatusPath"].(string))
	if _, err := os.Stat(firstRun["runnerLogPath"].(string)); err != nil {
		t.Fatalf("runner log missing: %v", err)
	}
	qaFixtureRoot := t.TempDir()
	qaScript := filepath.Join(qaFixtureRoot, "qa-command.sh")
	qaCount, qaReady, qaContinue := filepath.Join(qaFixtureRoot, "count"), filepath.Join(qaFixtureRoot, "ready"), filepath.Join(qaFixtureRoot, "continue.fifo")
	mustWrite(t, qaScript, []byte("#!/bin/sh\nprintf x >> \"$1\"\nmkfifo \"$3\"\nprintf ready > \"$2\"\nread line < \"$3\"\n"))
	if err := os.Chmod(qaScript, 0o700); err != nil {
		t.Fatal(err)
	}
	qaPacket := filepath.Join(qaFixtureRoot, "packet.json")
	packetContent, _ := json.Marshal(QAPacket{CandidateSHA: firstRun["baseSha"].(string), Criteria: []QACriterionSpec{{Name: "resumable process", Argv: []string{qaScript, qaCount, qaReady, qaContinue}}}})
	mustWrite(t, qaPacket, packetContent)
	qaArgs := []string{"qa", "run", "--project", projectID, "--state-root", stateRoot, "--run", firstRun["id"].(string), "--packet", qaPacket}
	qaProcess := exec.Command(binary, qaArgs...)
	var interruptedOutput bytes.Buffer
	qaProcess.Stdout, qaProcess.Stderr = &interruptedOutput, &interruptedOutput
	if err := qaProcess.Start(); err != nil {
		t.Fatal(err)
	}
	eventuallyReadStatus(t, qaReady)
	if err := qaProcess.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = qaProcess.Wait()
	if err := os.WriteFile(qaContinue, []byte("continue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	qaResult, exit := runCLI(binary, qaArgs...)
	if exit != 0 {
		t.Fatalf("QA resume exit=%d output=%s interrupted=%s", exit, qaResult, interruptedOutput.String())
	}
	count, err := os.ReadFile(qaCount)
	if err != nil {
		t.Fatal(err)
	}
	if string(count) != "x" {
		t.Fatalf("QA process ran more than once: %q", count)
	}

	runnerScript := filepath.Join(repository, "runner.sh")
	mustWrite(t, runnerScript, []byte("#!/bin/sh\ntrap 'printf cancelled > \"$NAGI_DERIVED_DATA/cancelled\"; exit 0' TERM INT\nprintf ready > \"$NAGI_DERIVED_DATA/ready\"\n/usr/bin/tail -f /dev/null &\nwait $!\n"))
	if err := os.Chmod(runnerScript, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repository, ".nagi.json"), []byte(`{"argv":["./runner.sh"]}`))
	runCommand(t, repository, "git", "add", ".nagi.json", "runner.sh")
	runCommand(t, repository, "git", "commit", "-m", "blocking runner fixture")
	if output, exit := runCLI(binary, "task", "add", "--project", projectID, "--state-root", stateRoot, "--id", "cancel-runner", "--title", "cancel runner", "--lane", "master", "--base", "master"); exit != 0 {
		t.Fatalf("add cancel task exit=%d output=%s", exit, output)
	}
	startResult, exit := runCLI(binary, "task", "start", "--project", projectID, "--state-root", stateRoot, "--task", "cancel-runner")
	if exit != 0 {
		t.Fatalf("start cancel runner exit=%d output=%s", exit, startResult)
	}
	derivedData := nestedString(t, startResult, "result", "derivedDataPath")
	runID := nestedString(t, startResult, "result", "id")
	eventuallyReadStatus(t, filepath.Join(derivedData, "ready"))
	cancelResult, exit := runCLI(binary, "run", "cancel", "--project", projectID, "--state-root", stateRoot, "--run", runID)
	if exit != 0 {
		t.Fatalf("cancel exit=%d output=%s", exit, cancelResult)
	}
	eventuallyReadStatus(t, filepath.Join(derivedData, "cancelled"))
}

func runCLI(binary string, args ...string) ([]byte, int) {
	command := exec.Command(binary, args...)
	output, err := command.CombinedOutput()
	if err == nil {
		return output, 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return output, exitError.ExitCode()
	}
	return output, -1
}

func nestedString(t *testing.T, content []byte, keys ...string) string {
	t.Helper()
	var value any
	if err := json.Unmarshal(content, &value); err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		value = value.(map[string]any)[key]
	}
	return value.(string)
}
