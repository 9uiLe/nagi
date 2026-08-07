//go:build xcode

package nagi

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestHostXcodeFixtureUsesDetachedSHAAndDedicatedDerivedData(t *testing.T) {
	status := (XcodeAdapter{}).HostStatus(context.Background())
	if !status.Available {
		t.Skipf("xcode unavailable: %s", status.Reason)
	}
	repository := filepath.Join(t.TempDir(), "xcode-repository")
	mustMkdir(t, repository)
	_, filename, _, _ := runtime.Caller(0)
	fixture := filepath.Join(filepath.Dir(filename), "testdata", "xcode-fixture")
	if err := copyTree(fixture, repository); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repository, ".nagi.json"), []byte(`{"argv":["/usr/bin/true"]}`))
	runCommand(t, repository, "git", "init", "-b", "master")
	runCommand(t, repository, "git", "config", "user.email", "nagi@example.test")
	runCommand(t, repository, "git", "config", "user.name", "Nagi Test")
	runCommand(t, repository, "git", "add", ".")
	runCommand(t, repository, "git", "commit", "-m", "xcode fixture")
	service := newTestService(t, repository)
	addReadyTask(t, service, "xcode-qa")
	run, err := service.StartTask(context.Background(), "xcode-qa", StartOptions{Actor: "implementation"})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := service.Git.RevParse(context.Background(), run.WorktreePath, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	service.Exec = OSExecutor{}
	packet := QAPacket{CandidateSHA: candidate, Xcode: "required", Criteria: []QACriterionSpec{{Name: "host xcode fixture", Fixture: "Package.swift", Argv: []string{"xcodebuild", "-scheme", "NagiFixture", "-destination", "platform=macOS", "test"}}}}
	report, err := service.RunQA(context.Background(), run.ID, packet, QAOptions{Actor: "independent-xcode-qa"})
	if err != nil {
		t.Fatal(err)
	}
	if report.QA.State != "passed" || report.QA.ValidatedSHA != candidate {
		t.Fatalf("%+v", report)
	}
	if report.QA.WorktreePath == run.WorktreePath || report.QA.DerivedDataPath == run.DerivedDataPath {
		t.Fatalf("QA isolation collapsed: %+v", report.QA)
	}
	if _, err := os.Stat(filepath.Join(report.QA.DerivedDataPath, "Build")); err != nil {
		t.Fatalf("dedicated DerivedData was not used: %v", err)
	}
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o600)
	})
}
