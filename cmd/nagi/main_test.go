package main

import (
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCLIHelpPrintsUsageAndExitsSuccessfully(t *testing.T) {
	_, filename, _, _ := runtime.Caller(0)
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	binary := filepath.Join(t.TempDir(), "nagi")
	build := exec.Command("go", "build", "-o", binary, "./cmd/nagi")
	build.Dir = moduleRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	tests := []struct {
		name      string
		args      []string
		usageFlag string
	}{
		{name: "init", args: []string{"init", "--help"}, usageFlag: "-repo string"},
		{name: "task add", args: []string{"task", "add", "--help"}, usageFlag: "-id string"},
		{name: "task start", args: []string{"task", "start", "--help"}, usageFlag: "-task string"},
		{name: "snapshot", args: []string{"snapshot", "--help"}, usageFlag: "-project string"},
		{name: "qa run", args: []string{"qa", "run", "--help"}, usageFlag: "-packet string"},
		{name: "run complete", args: []string{"run", "complete", "--help"}, usageFlag: "-disposition string"},
		{name: "pr prepare", args: []string{"pr", "prepare", "--help"}, usageFlag: "-target string"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command(binary, test.args...)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("help failed: %v\n%s", err, output)
			}
			usage := "Usage of " + test.name + ":\n"
			if !strings.HasPrefix(string(output), usage) {
				t.Fatalf("output does not start with %q:\n%s", usage, output)
			}
			if !strings.Contains(string(output), test.usageFlag) {
				t.Fatalf("output does not document %q:\n%s", test.usageFlag, output)
			}
		})
	}

	t.Run("flag errors remain JSON", func(t *testing.T) {
		command := exec.Command(binary, "init", "--unknown")
		output, err := command.CombinedOutput()
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
			t.Fatalf("exit=%v output=%s", err, output)
		}
		var payload struct {
			OK     bool   `json:"ok"`
			Reason string `json:"reason"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(output, &payload); err != nil {
			t.Fatalf("decode JSON: %v\n%s", err, output)
		}
		if payload.OK || payload.Reason != "internal_error" || payload.Error != "flag provided but not defined: -unknown" {
			t.Fatalf("payload=%+v", payload)
		}
	})
}
