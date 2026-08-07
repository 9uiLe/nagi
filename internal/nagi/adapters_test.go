package nagi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseWorktreePorcelainZNULSafe(t *testing.T) {
	data := []byte("worktree /repo/main\x00HEAD aaaaa\x00branch refs/heads/master\x00\x00worktree /repo/task with space\x00HEAD bbbbb\x00detached\x00locked nagi run run_1\x00\x00")
	got, err := ParseWorktreePorcelainZ(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []GitWorktree{{Path: "/repo/main", HEAD: "aaaaa", Branch: "refs/heads/master"}, {Path: "/repo/task with space", HEAD: "bbbbb", Locked: true, Reason: "nagi run run_1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestRunnerConfigRejectsUnsafeSeedPaths(t *testing.T) {
	for _, content := range []string{
		`{"argv":["true"],"seedFiles":["../secret"]}`,
		`{"argv":["true"],"seedFiles":["/secret"]}`,
	} {
		if _, err := LoadRunnerConfig([]byte(content)); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("got %v", err)
		}
	}
}

func TestCopySeedFilesKeepsContentOutOfMetadata(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(source, "seed.txt"), []byte("top-secret-value"))
	if err := CopySeedFiles(source, destination, []string{"seed.txt"}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(destination, "seed.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "top-secret-value" {
		t.Fatalf("unexpected content %q", content)
	}
}

func TestCopySeedFilesRejectsDestinationParentSymlinkEscape(t *testing.T) {
	source, destination, outside := t.TempDir(), t.TempDir(), t.TempDir()
	mustMkdir(t, filepath.Join(source, "escape"))
	mustWrite(t, filepath.Join(source, "escape", "seed.txt"), []byte("do not escape"))
	if err := os.Symlink(outside, filepath.Join(destination, "escape")); err != nil {
		t.Fatal(err)
	}
	err := CopySeedFiles(source, destination, []string{"escape/seed.txt"})
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "seed.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("seed escaped destination root: %v", err)
	}
}

func TestXcodeHostStatusReturnsMachineReadableReason(t *testing.T) {
	executor := &fakeExecutor{results: map[string]CommandResult{"xcode-select": {ExitStatus: 1, Stderr: "not selected"}}}
	status := (XcodeAdapter{Exec: executor}).HostStatus(context.Background())
	if status.Available || status.Reason != "xcode_not_selected" {
		t.Fatalf("%+v", status)
	}
}

func TestXcodeCommandGetsRunScopedDerivedData(t *testing.T) {
	got := AddDerivedDataArg([]string{"xcodebuild", "test", "-scheme", "Fixture"}, "/qa/DerivedData")
	want := []string{"xcodebuild", "test", "-scheme", "Fixture", "-derivedDataPath", "/qa/DerivedData"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}
