package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/9uile/nagi/internal/nagi"
)

type commandOutput struct {
	Value any
	Error error
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "__runner-exec" {
		os.Exit(runWrapped(os.Args[2:]))
	}
	output := execute(context.Background(), os.Args[1:])
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	if output.Error == nil {
		_ = encoder.Encode(map[string]any{"ok": true, "result": output.Value})
		return
	}
	payload := map[string]any{"ok": false, "reason": reason(output.Error), "error": output.Error.Error()}
	if output.Value != nil {
		payload["details"] = output.Value
	}
	_ = encoder.Encode(payload)
	os.Exit(exitCode(output.Error))
}

func execute(ctx context.Context, args []string) commandOutput {
	if len(args) == 0 {
		return commandOutput{Error: fmt.Errorf("command is required")}
	}
	switch args[0] {
	case "init":
		return initCommand(ctx, args[1:])
	case "host-check":
		return hostCheckCommand(ctx, args[1:])
	case "db":
		return dbCommand(ctx, args[1:])
	case "task":
		return taskCommand(ctx, args[1:])
	case "seed":
		return seedCommand(ctx, args[1:])
	case "resume":
		return resumeCommand(ctx, args[1:])
	case "snapshot":
		return snapshotCommand(ctx, args[1:])
	case "reconcile":
		return reconcileCommand(ctx, args[1:])
	case "run":
		return runCommand(ctx, args[1:])
	case "cleanup":
		return cleanupCommand(ctx, args[1:])
	case "qa":
		return qaCommand(ctx, args[1:])
	case "pr":
		return prCommand(ctx, args[1:])
	case "events":
		return eventsCommand(ctx, args[1:])
	default:
		return commandOutput{Error: fmt.Errorf("unknown command %q", args[0])}
	}
}

func initCommand(ctx context.Context, args []string) commandOutput {
	flags := newFlags("init")
	repository := flags.String("repo", ".", "Git repository")
	stateRoot := flags.String("state-root", "", "external state root")
	config := flags.String("config", ".nagi.json", "trusted runner config path in the base commit")
	if err := flags.Parse(args); err != nil {
		return commandOutput{Error: err}
	}
	project, verification, err := nagi.Initialize(ctx, *repository, *stateRoot, *config, nil, nil)
	return commandOutput{Value: map[string]any{"project": project, "database": verification}, Error: err}
}

func hostCheckCommand(ctx context.Context, args []string) commandOutput {
	flags := newFlags("host-check")
	component := flags.String("component", "xcode", "xcode or github")
	if err := flags.Parse(args); err != nil {
		return commandOutput{Error: err}
	}
	var status nagi.HostStatus
	switch *component {
	case "xcode":
		status = (nagi.XcodeAdapter{}).HostStatus(ctx)
	case "github":
		status = (nagi.GHAdapter{}).HostStatus(ctx)
	default:
		return commandOutput{Error: fmt.Errorf("unknown component %q", *component)}
	}
	if !status.Available {
		return commandOutput{Value: status, Error: fmt.Errorf("%s: %w", status.Reason, nagi.ErrPrerequisite)}
	}
	return commandOutput{Value: status}
}

func dbCommand(ctx context.Context, args []string) commandOutput {
	if len(args) == 0 || args[0] != "verify" {
		return commandOutput{Error: fmt.Errorf("db verify is required")}
	}
	service, output := openService(ctx, "db verify", args[1:])
	if output.Error != nil {
		return output
	}
	defer service.Close()
	verification, err := service.Store.Verify(ctx)
	return commandOutput{Value: verification, Error: err}
}

func taskCommand(ctx context.Context, args []string) commandOutput {
	if len(args) == 0 {
		return commandOutput{Error: fmt.Errorf("task subcommand is required")}
	}
	switch args[0] {
	case "add":
		flags := newFlags("task add")
		projectID, stateRoot, actor := commonFlags(flags)
		id := flags.String("id", "", "task ID")
		title := flags.String("title", "", "task title")
		parent := flags.String("parent", "", "parent task ID")
		dependency := flags.String("depends", "", "execution dependency task ID")
		lane := flags.String("lane", "base", "integration lane")
		base := flags.String("base", "master", "base ref")
		if err := flags.Parse(args[1:]); err != nil {
			return commandOutput{Error: err}
		}
		service, err := nagi.OpenService(ctx, *projectID, *stateRoot, nil, nil)
		if err != nil {
			return commandOutput{Error: err}
		}
		defer service.Close()
		task, err := service.AddTask(ctx, nagi.Task{ID: *id, Title: *title, ParentID: *parent, DependencyID: *dependency, IntegrationLane: *lane, BaseRef: *base}, *actor)
		if err != nil {
			return commandOutput{Error: err}
		}
		return commandOutput{Value: task}
	case "list":
		service, output := openService(ctx, "task list", args[1:])
		if output.Error != nil {
			return output
		}
		defer service.Close()
		tasks, err := service.Store.Tasks(ctx, service.Project.ID)
		return commandOutput{Value: tasks, Error: err}
	case "start":
		flags := newFlags("task start")
		projectID, stateRoot, actor := commonFlags(flags)
		taskID := flags.String("task", "", "task ID")
		fault := flags.String("fault-after", "", "fault injection point")
		if err := flags.Parse(args[1:]); err != nil {
			return commandOutput{Error: err}
		}
		service, err := nagi.OpenService(ctx, *projectID, *stateRoot, nil, nil)
		if err != nil {
			return commandOutput{Error: err}
		}
		defer service.Close()
		run, err := service.StartTask(ctx, *taskID, nagi.StartOptions{Actor: *actor, FaultAfter: *fault})
		if errors.Is(err, nagi.ErrAlreadyClaimed) {
			existing, lookupErr := service.Store.RunForTask(ctx, *taskID)
			if lookupErr == nil {
				return commandOutput{Value: map[string]any{"run": existing}, Error: err}
			}
		}
		return commandOutput{Value: run, Error: err}
	default:
		return commandOutput{Error: fmt.Errorf("unknown task subcommand %q", args[0])}
	}
}

func seedCommand(ctx context.Context, args []string) commandOutput {
	if len(args) == 0 || args[0] != "register" {
		return commandOutput{Error: fmt.Errorf("seed register is required")}
	}
	flags := newFlags("seed register")
	projectID, stateRoot, _ := commonFlags(flags)
	source := flags.String("source", "", "source file")
	name := flags.String("name", "", "registered relative seed name")
	if err := flags.Parse(args[1:]); err != nil {
		return commandOutput{Error: err}
	}
	service, err := nagi.OpenService(ctx, *projectID, *stateRoot, nil, nil)
	if err != nil {
		return commandOutput{Error: err}
	}
	defer service.Close()
	path, err := service.RegisterSeed(*source, *name)
	return commandOutput{Value: map[string]string{"name": *name, "path": path}, Error: err}
}

func resumeCommand(ctx context.Context, args []string) commandOutput {
	flags := newFlags("resume")
	projectID, stateRoot, actor := commonFlags(flags)
	if err := flags.Parse(args); err != nil {
		return commandOutput{Error: err}
	}
	service, err := nagi.OpenService(ctx, *projectID, *stateRoot, nil, nil)
	if err != nil {
		return commandOutput{Error: err}
	}
	defer service.Close()
	runs, err := service.ResumeProvisioning(ctx, *actor)
	return commandOutput{Value: runs, Error: err}
}

func snapshotCommand(ctx context.Context, args []string) commandOutput {
	service, output := openService(ctx, "snapshot", args)
	if output.Error != nil {
		return output
	}
	defer service.Close()
	snapshot, err := service.Snapshot(ctx)
	return commandOutput{Value: snapshot, Error: err}
}

func reconcileCommand(ctx context.Context, args []string) commandOutput {
	flags := newFlags("reconcile")
	projectID, stateRoot, actor := commonFlags(flags)
	if err := flags.Parse(args); err != nil {
		return commandOutput{Error: err}
	}
	service, err := nagi.OpenService(ctx, *projectID, *stateRoot, nil, nil)
	if err != nil {
		return commandOutput{Error: err}
	}
	defer service.Close()
	resumed, err := service.ResumeProvisioning(ctx, *actor)
	if err != nil {
		return commandOutput{Error: err}
	}
	findings, err := service.ReconcileWorktrees(ctx)
	return commandOutput{Value: map[string]any{"resumedRuns": resumed, "findings": findings}, Error: err}
}

func runCommand(ctx context.Context, args []string) commandOutput {
	if len(args) == 0 {
		return commandOutput{Error: fmt.Errorf("run subcommand is required")}
	}
	flags := newFlags("run " + args[0])
	projectID, stateRoot, actor := commonFlags(flags)
	runID := flags.String("run", "", "run ID")
	disposition := flags.String("disposition", "", "integrated or discarded")
	if err := flags.Parse(args[1:]); err != nil {
		return commandOutput{Error: err}
	}
	service, err := nagi.OpenService(ctx, *projectID, *stateRoot, nil, nil)
	if err != nil {
		return commandOutput{Error: err}
	}
	defer service.Close()
	switch args[0] {
	case "cancel":
		run, err := service.CancelRun(ctx, *runID, *actor)
		return commandOutput{Value: run, Error: err}
	case "complete":
		run, err := service.CompleteRun(ctx, *runID, *disposition, *actor)
		return commandOutput{Value: run, Error: err}
	default:
		return commandOutput{Error: fmt.Errorf("unknown run subcommand %q", args[0])}
	}
}

func cleanupCommand(ctx context.Context, args []string) commandOutput {
	flags := newFlags("cleanup")
	projectID, stateRoot, actor := commonFlags(flags)
	runID := flags.String("run", "", "run ID")
	fault := flags.String("fault-after", "", "fault injection step")
	if err := flags.Parse(args); err != nil {
		return commandOutput{Error: err}
	}
	service, err := nagi.OpenService(ctx, *projectID, *stateRoot, nil, nil)
	if err != nil {
		return commandOutput{Error: err}
	}
	defer service.Close()
	result, err := service.CleanupRun(ctx, *runID, nagi.CleanupOptions{Actor: *actor, FaultAfter: *fault})
	return commandOutput{Value: result, Error: err}
}

func qaCommand(ctx context.Context, args []string) commandOutput {
	if len(args) == 0 || args[0] != "run" {
		return commandOutput{Error: fmt.Errorf("qa run is required")}
	}
	flags := newFlags("qa run")
	projectID, stateRoot, actor := commonFlags(flags)
	runID := flags.String("run", "", "implementation run ID")
	packetPath := flags.String("packet", "", "QA packet JSON")
	fault := flags.String("fault-after", "", "criterion name for fault injection")
	if err := flags.Parse(args[1:]); err != nil {
		return commandOutput{Error: err}
	}
	content, err := os.ReadFile(*packetPath)
	if err != nil {
		return commandOutput{Error: err}
	}
	packet, err := nagi.DecodeQAPacket(content)
	if err != nil {
		return commandOutput{Error: err}
	}
	service, err := nagi.OpenService(ctx, *projectID, *stateRoot, nil, nil)
	if err != nil {
		return commandOutput{Error: err}
	}
	defer service.Close()
	report, err := service.RunQA(ctx, *runID, packet, nagi.QAOptions{Actor: *actor, FaultAfter: *fault})
	return commandOutput{Value: report, Error: err}
}

func prCommand(ctx context.Context, args []string) commandOutput {
	if len(args) == 0 {
		return commandOutput{Error: fmt.Errorf("pr subcommand is required")}
	}
	flags := newFlags("pr " + args[0])
	projectID, stateRoot, actor := commonFlags(flags)
	runID := flags.String("run", "", "run ID")
	target := flags.String("target", "", "target branch")
	if err := flags.Parse(args[1:]); err != nil {
		return commandOutput{Error: err}
	}
	service, err := nagi.OpenService(ctx, *projectID, *stateRoot, nil, nil)
	if err != nil {
		return commandOutput{Error: err}
	}
	defer service.Close()
	switch args[0] {
	case "prepare":
		pr, err := service.PreparePullRequest(ctx, *runID, *target, *actor)
		return commandOutput{Value: pr, Error: err}
	case "sync":
		pr, changed, err := service.SyncPullRequest(ctx, *runID, *actor)
		return commandOutput{Value: map[string]any{"pullRequest": pr, "changed": changed}, Error: err}
	case "undraft":
		decision, err := service.UndraftPullRequest(ctx, *runID, *actor)
		return commandOutput{Value: decision, Error: err}
	default:
		return commandOutput{Error: fmt.Errorf("unknown pr subcommand %q", args[0])}
	}
}

func eventsCommand(ctx context.Context, args []string) commandOutput {
	service, output := openService(ctx, "events", args)
	if output.Error != nil {
		return output
	}
	defer service.Close()
	events, err := service.Store.Events(ctx, service.Project.ID)
	return commandOutput{Value: events, Error: err}
}

func openService(ctx context.Context, name string, args []string) (*nagi.Service, commandOutput) {
	flags := newFlags(name)
	projectID, stateRoot, _ := commonFlags(flags)
	if err := flags.Parse(args); err != nil {
		return nil, commandOutput{Error: err}
	}
	service, err := nagi.OpenService(ctx, *projectID, *stateRoot, nil, nil)
	return service, commandOutput{Error: err}
}

func commonFlags(flags *flag.FlagSet) (*string, *string, *string) {
	projectID := flags.String("project", "", "project ID")
	stateRoot := flags.String("state-root", "", "external state root")
	actor := flags.String("actor", "cli", "audit actor")
	return projectID, stateRoot, actor
}

func newFlags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func reason(err error) string {
	switch {
	case errors.Is(err, nagi.ErrAlreadyClaimed):
		return "already_claimed"
	case errors.Is(err, nagi.ErrNotReady):
		return "not_ready"
	case errors.Is(err, nagi.ErrNotFound):
		return "not_found"
	case errors.Is(err, nagi.ErrPrerequisite):
		return "prerequisite_unavailable"
	case errors.Is(err, nagi.ErrCleanupBlocked):
		return "cleanup_blocked"
	case errors.Is(err, nagi.ErrUndraftBlocked):
		return "undraft_blocked"
	case errors.Is(err, nagi.ErrInjectedFault):
		return "injected_fault"
	case errors.Is(err, nagi.ErrUnsafePath):
		return "unsafe_path"
	case errors.Is(err, nagi.ErrInvalidArgument):
		return "invalid_arguments"
	case errors.Is(err, nagi.ErrInvalidState):
		return "invalid_state"
	default:
		if strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "unknown command") {
			return "invalid_arguments"
		}
		return "internal_error"
	}
}

func exitCode(err error) int {
	switch {
	case errors.Is(err, nagi.ErrAlreadyClaimed):
		return 10
	case errors.Is(err, nagi.ErrPrerequisite):
		return 11
	case errors.Is(err, nagi.ErrCleanupBlocked), errors.Is(err, nagi.ErrUndraftBlocked), errors.Is(err, nagi.ErrNotReady):
		return 12
	case reason(err) == "invalid_arguments":
		return 2
	default:
		return 1
	}
}

func runWrapped(args []string) int {
	statusPath := ""
	separator := -1
	for index, arg := range args {
		if arg == "--status" && index+1 < len(args) {
			statusPath = args[index+1]
		}
		if arg == "--" {
			separator = index
			break
		}
	}
	if statusPath == "" || separator < 0 || separator+1 >= len(args) {
		return 2
	}
	if err := nagi.RunWrappedProcess(statusPath, args[separator+1:]); err != nil {
		return 1
	}
	return 0
}
