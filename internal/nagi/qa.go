package nagi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type QAOptions struct {
	Actor      string
	FaultAfter string
}

type QAReport struct {
	QA       QARun               `json:"qa"`
	Criteria []QACriterionResult `json:"criteria"`
}

func DecodeQAPacket(content []byte) (QAPacket, error) {
	var packet QAPacket
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&packet); err != nil {
		return packet, err
	}
	if packet.CandidateSHA == "" || len(packet.Criteria) == 0 {
		return packet, fmt.Errorf("candidateSha and criteria are required")
	}
	for _, criterion := range packet.Criteria {
		if criterion.Name == "" || len(criterion.Argv) == 0 {
			return packet, fmt.Errorf("criterion name and argv are required")
		}
		if criterion.Fixture != "" {
			if err := validateRelative(criterion.Fixture); err != nil {
				return packet, err
			}
		}
		for _, artifact := range criterion.Artifacts {
			if err := validateRelative(artifact); err != nil {
				return packet, err
			}
		}
	}
	return packet, nil
}

func (s *Service) RunQA(ctx context.Context, runID string, packet QAPacket, options QAOptions) (QAReport, error) {
	run, err := s.Store.Run(ctx, runID)
	if err != nil {
		return QAReport{}, err
	}
	candidate, err := s.Git.RevParse(ctx, s.Project.Repository, packet.CandidateSHA)
	if err != nil {
		return QAReport{}, err
	}
	if candidate != packet.CandidateSHA {
		packet.CandidateSHA = candidate
	}
	selectedXcode := ""
	requiresXcode := false
	for _, criterion := range packet.Criteria {
		if filepath.Base(criterion.Argv[0]) == "xcodebuild" {
			requiresXcode = true
		}
	}
	if requiresXcode || packet.Xcode == "required" {
		status := (XcodeAdapter{Exec: s.Exec}).HostStatus(ctx)
		if !status.Available {
			return QAReport{}, fmt.Errorf("%s: %w", status.Reason, ErrPrerequisite)
		}
		selectedXcode = status.Selected
	}
	qa, lookupErr := s.Store.QARunByCandidate(ctx, run.ID, candidate)
	if lookupErr != nil && !errors.Is(lookupErr, ErrNotFound) {
		return QAReport{}, lookupErr
	}
	if errors.Is(lookupErr, ErrNotFound) {
		qaID := s.IDs.NewID("qa")
		qa = QARun{ID: qaID, RunID: run.ID, CandidateSHA: candidate, State: "planned", WorktreePath: filepath.Join(s.Project.StateDir, "qa-worktrees", qaID), DerivedDataPath: filepath.Join(s.Project.StateDir, "qa-derived-data", qaID), ArtifactDir: filepath.Join(s.Project.StateDir, "artifacts", "qa", qaID), CreatedAt: s.Clock.Now(), UpdatedAt: s.Clock.Now()}
	} else if qa.SelectedXcode != "" {
		selectedXcode = qa.SelectedXcode
	}
	packet.SelectedXcode = selectedXcode
	packet.ArtifactDestination = qa.ArtifactDir
	packetJSON, _ := json.Marshal(packet)
	if lookupErr == nil && !bytes.Equal(qa.Packet, packetJSON) {
		return QAReport{}, fmt.Errorf("QA packet changed for the same candidate: %w", ErrInvalidState)
	}
	qa.Packet = packetJSON
	qa, err = s.Store.EnsureQARun(ctx, qa)
	if err != nil {
		return QAReport{}, err
	}
	op := Operation{ID: s.IDs.NewID("operation"), RunID: run.ID, Kind: "qa/" + candidate, DesiredState: "validated", ObservedState: "planned", Details: packetJSON, CreatedAt: s.Clock.Now(), UpdatedAt: s.Clock.Now()}
	op, err = s.Store.EnsureOperation(ctx, op)
	if err != nil {
		return QAReport{}, err
	}
	if err := s.Git.AddDetachedWorktree(ctx, s.Project.Repository, qa.WorktreePath, candidate, "nagi qa "+qa.ID); err != nil {
		return QAReport{}, err
	}
	if err := os.MkdirAll(qa.DerivedDataPath, 0o700); err != nil {
		return QAReport{}, err
	}
	if err := os.MkdirAll(qa.ArtifactDir, 0o700); err != nil {
		return QAReport{}, err
	}
	if err := s.Store.CompleteStep(ctx, s.Project.ID, options.Actor, op, "qa_isolation", nil, map[string]any{"candidateSha": candidate, "worktree": qa.WorktreePath, "derivedData": qa.DerivedDataPath}); err != nil {
		return QAReport{}, err
	}
	qa, err = s.Store.UpdateQARun(ctx, qa, "running", selectedXcode, "", s.Project.ID, options.Actor, op.ID)
	if err != nil {
		return QAReport{}, err
	}
	for _, criterion := range packet.Criteria {
		existing, criterionErr := s.Store.QACriterion(ctx, qa.ID, criterion.Name)
		if criterionErr != nil && !errors.Is(criterionErr, ErrNotFound) {
			return QAReport{}, criterionErr
		}
		if criterionErr == nil && (existing.Status == "pass" || existing.Status == "fail") {
			continue
		}
		logPath := filepath.Join(qa.ArtifactDir, sanitizeName(criterion.Name)+".log")
		var execution struct {
			Argv       []string `json:"argv"`
			CWD        string   `json:"cwd"`
			ExitStatus int      `json:"exitStatus"`
			Candidate  string   `json:"candidateSha"`
		}
		artifactRefs := []string{logPath}
		if criterionErr == nil && existing.Status == "executed" {
			if err := json.Unmarshal(existing.Observed, &execution); err != nil {
				return QAReport{}, err
			}
			artifactRefs = existing.Artifacts
		} else {
			argv := AddDerivedDataArg(criterion.Argv, qa.DerivedDataPath)
			environment := qaEnvironment(run, qa, selectedXcode)
			executionID := op.ID + "/" + sanitizeName(criterion.Name)
			executionStatus := filepath.Join(qa.ArtifactDir, "executions", sanitizeName(criterion.Name)+".status.json")
			executionLog := filepath.Join(qa.ArtifactDir, sanitizeName(criterion.Name)+".log")
			planned := map[string]any{"executionId": executionID, "argv": argv, "cwd": qa.WorktreePath, "candidateSha": candidate, "statusPath": executionStatus, "logPath": executionLog}
			if step, stepErr := s.Store.Step(ctx, op.ID, "criterion/"+criterion.Name+"/process_planned"); stepErr != nil || step.State != "completed" {
				if stepErr != nil && !errors.Is(stepErr, ErrNotFound) {
					return QAReport{}, stepErr
				}
				if err := s.Store.CompleteStep(ctx, s.Project.ID, options.Actor, op, "criterion/"+criterion.Name+"/process_planned", nil, planned); err != nil {
					return QAReport{}, err
				}
			}
			if options.FaultAfter == criterion.Name+"/planned" {
				return QAReport{QA: qa}, ErrInjectedFault
			}
			var result CommandResult
			var runErr error
			if resumable, ok := s.Exec.(ResumableCommandExecutor); ok {
				result, runErr = resumable.RunResumable(ctx, ResumableCommand{ID: executionID, CWD: qa.WorktreePath, Environment: environment, Argv: argv, StatusPath: executionStatus, LogPath: executionLog})
			} else {
				result, runErr = s.Exec.Run(ctx, qa.WorktreePath, environment, argv...)
			}
			if runErr != nil {
				return QAReport{}, runErr
			}
			logContent := []byte(result.Stdout + result.Stderr)
			if err := os.WriteFile(logPath, logContent, 0o600); err != nil {
				return QAReport{}, err
			}
			execution = struct {
				Argv       []string `json:"argv"`
				CWD        string   `json:"cwd"`
				ExitStatus int      `json:"exitStatus"`
				Candidate  string   `json:"candidateSha"`
			}{Argv: result.Argv, CWD: result.CWD, ExitStatus: result.ExitStatus, Candidate: candidate}
			observed, _ := json.Marshal(execution)
			executed := QACriterionResult{QAID: qa.ID, Name: criterion.Name, Status: "executed", Observed: observed, Reproducibility: strings.Join(result.Argv, " "), Artifacts: artifactRefs}
			if err := s.Store.SaveQACriterion(ctx, executed); err != nil {
				return QAReport{}, err
			}
			if err := s.Store.CompleteStep(ctx, s.Project.ID, options.Actor, op, "criterion/"+criterion.Name+"/process", nil, executed); err != nil {
				return QAReport{}, err
			}
			if options.FaultAfter == criterion.Name+"/process" {
				return QAReport{QA: qa}, ErrInjectedFault
			}
		}
		status := "pass"
		if execution.ExitStatus != 0 {
			status = "fail"
		}
		for _, artifact := range criterion.Artifacts {
			source, err := containedPath(qa.WorktreePath, artifact, true)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrUnsafePath) {
					status = "fail"
					continue
				}
				return QAReport{}, err
			}
			destination := filepath.Join(qa.ArtifactDir, sanitizeName(criterion.Name)+"-"+filepath.Base(artifact))
			if err := copyArtifact(source, destination); err != nil {
				return QAReport{}, err
			}
			artifactRefs = append(artifactRefs, destination)
		}
		observed, _ := json.Marshal(execution)
		criterionResult := QACriterionResult{QAID: qa.ID, Name: criterion.Name, Status: status, Observed: observed, Reproducibility: strings.Join(execution.Argv, " "), Artifacts: artifactRefs}
		if err := s.Store.SaveQACriterion(ctx, criterionResult); err != nil {
			return QAReport{}, err
		}
		if err := s.Store.CompleteStep(ctx, s.Project.ID, options.Actor, op, "criterion/"+criterion.Name+"/evidence", existing, criterionResult); err != nil {
			return QAReport{}, err
		}
		if options.FaultAfter == criterion.Name {
			return QAReport{QA: qa}, ErrInjectedFault
		}
	}
	results, err := s.Store.QACriteria(ctx, qa.ID)
	if err != nil {
		return QAReport{}, err
	}
	allPassed := len(results) == len(packet.Criteria)
	for _, result := range results {
		if result.Status != "pass" {
			allPassed = false
		}
	}
	state, validated := "failed", ""
	if allPassed {
		state, validated = "passed", candidate
	}
	qa, err = s.Store.UpdateQARun(ctx, qa, state, selectedXcode, validated, s.Project.ID, options.Actor, op.ID)
	if err != nil {
		return QAReport{}, err
	}
	details, _ := json.Marshal(map[string]any{"qaId": qa.ID, "state": state, "validatedSha": validated})
	observed := "failed"
	if allPassed {
		observed = "validated"
	}
	if _, err := s.Store.UpdateOperation(ctx, s.Project.ID, options.Actor, op, observed, op.Cursor, details); err != nil {
		return QAReport{}, err
	}
	return QAReport{QA: qa, Criteria: results}, nil
}

func qaEnvironment(run Run, qa QARun, selectedXcode string) []string {
	environment := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.Getenv("TMPDIR"), "NAGI_RUN_ID=" + run.ID, "NAGI_QA_ID=" + qa.ID, "NAGI_WORKTREE=" + qa.WorktreePath, "NAGI_DERIVED_DATA=" + qa.DerivedDataPath}
	if selectedXcode != "" {
		environment = append(environment, "DEVELOPER_DIR="+selectedXcode)
	}
	return environment
}

func copyArtifact(source, destination string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			target := filepath.Join(destination, relative)
			if entry.Type()&os.ModeSymlink != 0 {
				return ErrUnsafePath
			}
			if entry.IsDir() {
				return os.MkdirAll(target, 0o700)
			}
			entryInfo, err := entry.Info()
			if err != nil {
				return err
			}
			if !entryInfo.Mode().IsRegular() {
				return fmt.Errorf("artifact contains a non-regular file")
			}
			return copyArtifactFile(path, target)
		})
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("artifact is not a regular file or directory")
	}
	return copyArtifactFile(source, destination)
}

func copyArtifactFile(source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	return errors.Join(copyErr, closeErr)
}

func sanitizeName(value string) string {
	var builder strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 {
		return "criterion"
	}
	return builder.String()
}
