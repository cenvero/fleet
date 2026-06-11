// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ExecFn runs a command on a server and returns its exit code, captured stdout
// and stderr, and a transport-level error (nil when the command ran, even if it
// exited non-zero). It is a function type so the playbook engine can be driven
// by a fake in tests without depending on *App.
type ExecFn func(server, command string) (exitCode int, stdout, stderr string, err error)

// PlaybookStep is one ordered unit of work. Each field is a shell command (run
// on the target server) and may be empty.
//
//   - Check:    if non-empty and it exits 0, the step is already satisfied and
//     Apply is skipped (idempotency).
//   - Apply:    the command that brings the step into the desired state.
//   - Rollback: the command used to undo Apply when a LATER step fails and
//     rollback-on-failure is enabled.
type PlaybookStep struct {
	Name     string `yaml:"name"`
	Check    string `yaml:"check"`
	Apply    string `yaml:"apply"`
	Rollback string `yaml:"rollback"`
}

// Playbook is a parsed playbook document.
type Playbook struct {
	Name string `yaml:"name"`
	// Hosts is an optional default target expression (a tag filter like
	// "role=plesk" or "role=plesk,env=prod"). It is only used when no explicit
	// target is given on the command line.
	Hosts string         `yaml:"hosts"`
	Steps []PlaybookStep `yaml:"steps"`
}

// ParsePlaybook decodes a playbook from YAML bytes and validates it.
func ParsePlaybook(data []byte) (Playbook, error) {
	var pb Playbook
	if err := yaml.Unmarshal(data, &pb); err != nil {
		return Playbook{}, fmt.Errorf("parse playbook: %w", err)
	}
	if err := pb.validate(); err != nil {
		return Playbook{}, err
	}
	return pb, nil
}

// LoadPlaybook reads and parses a playbook file.
func LoadPlaybook(path string) (Playbook, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Playbook{}, fmt.Errorf("read playbook: %w", err)
	}
	return ParsePlaybook(data)
}

func (pb Playbook) validate() error {
	if strings.TrimSpace(pb.Name) == "" {
		return fmt.Errorf("playbook name is required")
	}
	if len(pb.Steps) == 0 {
		return fmt.Errorf("playbook %q has no steps", pb.Name)
	}
	for i, step := range pb.Steps {
		if strings.TrimSpace(step.Name) == "" {
			return fmt.Errorf("playbook %q step %d has no name", pb.Name, i+1)
		}
		if strings.TrimSpace(step.Apply) == "" {
			return fmt.Errorf("playbook %q step %q has no apply command", pb.Name, step.Name)
		}
	}
	return nil
}

// Step status values recorded per server per step.
const (
	// StepSatisfied: the check command exited 0, so apply was not needed.
	StepSatisfied = "satisfied"
	// StepApplied: apply ran and succeeded.
	StepApplied = "applied"
	// StepSkipped: the step did not run because an earlier step failed.
	StepSkipped = "skipped"
	// StepFailed: apply ran and exited non-zero (or errored).
	StepFailed = "failed"
	// StepRolledBack: a previously-applied step whose rollback command was
	// actually executed.
	StepRolledBack = "rolledback"
	// StepRollbackSkipped: a previously-applied step that has no rollback
	// command, so no undo was attempted. The step's apply result is preserved.
	StepRollbackSkipped = "rollback-skipped"
)

// RunOptions controls playbook execution.
type RunOptions struct {
	// OnFailRollback, when true, runs the rollback of each previously-applied
	// (not skipped/satisfied) step in reverse order when a step fails.
	OnFailRollback bool
	// DryRun resolves and returns the plan without executing anything.
	DryRun bool
}

// StepResult is the outcome of a single step on a single server.
type StepResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// ExitCode is the apply (or check, when satisfied) command's exit code.
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	// Detail carries a transport/engine error message when present.
	Detail string `json:"detail,omitempty"`
	// Rollback holds the result of this step's rollback command when one was
	// executed (status StepRolledBack). It is nil otherwise, so the apply
	// fields above always reflect the original apply attempt.
	Rollback *RollbackResult `json:"rollback,omitempty"`
}

// RollbackResult captures the outcome of a step's rollback command. It is kept
// separate from the apply fields so the original apply failure output is not
// discarded when an undo runs.
type RollbackResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	// Detail carries a transport/engine error message from the rollback command.
	Detail string `json:"detail,omitempty"`
}

// ServerResult is the outcome of running a whole playbook on one server.
type ServerResult struct {
	Server string       `json:"server"`
	Steps  []StepResult `json:"steps"`
	// Failed is true if any step failed on this server.
	Failed bool `json:"failed"`
	// Err is a short message describing the failure (empty on success).
	Err string `json:"error,omitempty"`
}

// PlaybookResult is the aggregate outcome across all targeted servers.
type PlaybookResult struct {
	Playbook string         `json:"playbook"`
	DryRun   bool           `json:"dry_run"`
	Servers  []ServerResult `json:"servers"`
}

// Failed reports whether any server failed.
func (r PlaybookResult) Failed() bool {
	for _, s := range r.Servers {
		if s.Failed {
			return true
		}
	}
	return false
}

// RunPlaybook executes a playbook across the given servers using exec.
//
// For each server, steps run IN ORDER:
//   - If Check is non-empty, run it. Exit 0 => already satisfied => SKIP apply
//     (recorded as "satisfied"); the step is NOT eligible for rollback.
//   - Otherwise run Apply. A non-zero exit (or transport error) marks the step
//     "failed"; if OnFailRollback is set, the rollback of each PREVIOUSLY-APPLIED
//     step (not satisfied, not skipped) runs in REVERSE order. A step whose
//     rollback command runs is recorded as "rolledback" (with its rollback
//     outcome kept separately, preserving the apply result); a step without a
//     rollback command is recorded as "rollback-skipped". Execution then stops
//     for that server.
//
// Servers are independent; a failure on one does not stop the others.
//
// DryRun returns the resolved plan (every step "skipped") and runs nothing.
func RunPlaybook(exec ExecFn, pb Playbook, servers []string, opts RunOptions) PlaybookResult {
	result := PlaybookResult{Playbook: pb.Name, DryRun: opts.DryRun}
	for _, server := range servers {
		if opts.DryRun {
			result.Servers = append(result.Servers, dryRunServer(server, pb))
			continue
		}
		result.Servers = append(result.Servers, runPlaybookOnServer(exec, pb, server, opts))
	}
	return result
}

// dryRunServer returns the plan for one server without executing anything.
func dryRunServer(server string, pb Playbook) ServerResult {
	sr := ServerResult{Server: server}
	for _, step := range pb.Steps {
		sr.Steps = append(sr.Steps, StepResult{Name: step.Name, Status: StepSkipped})
	}
	return sr
}

// appliedStep tracks a step that actually applied, so rollback can target it.
type appliedStep struct {
	index int
	step  PlaybookStep
}

func runPlaybookOnServer(exec ExecFn, pb Playbook, server string, opts RunOptions) ServerResult {
	sr := ServerResult{Server: server, Steps: make([]StepResult, len(pb.Steps))}
	var applied []appliedStep

	for i, step := range pb.Steps {
		// Idempotency: a passing check means the step is already satisfied.
		if strings.TrimSpace(step.Check) != "" {
			code, stdout, stderr, err := exec(server, step.Check)
			if err == nil && code == 0 {
				sr.Steps[i] = StepResult{
					Name:     step.Name,
					Status:   StepSatisfied,
					ExitCode: code,
					Stdout:   stdout,
					Stderr:   stderr,
				}
				continue
			}
		}

		// Apply the step.
		code, stdout, stderr, err := exec(server, step.Apply)
		if err != nil || code != 0 {
			res := StepResult{
				Name:     step.Name,
				Status:   StepFailed,
				ExitCode: code,
				Stdout:   stdout,
				Stderr:   stderr,
			}
			if err != nil {
				res.Detail = err.Error()
			}
			sr.Steps[i] = res
			sr.Failed = true
			sr.Err = fmt.Sprintf("step %q failed", step.Name)

			// Mark the remaining steps as skipped.
			for j := i + 1; j < len(pb.Steps); j++ {
				sr.Steps[j] = StepResult{Name: pb.Steps[j].Name, Status: StepSkipped}
			}

			if opts.OnFailRollback {
				rollbackApplied(exec, server, applied, sr.Steps)
			}
			return sr
		}

		sr.Steps[i] = StepResult{
			Name:     step.Name,
			Status:   StepApplied,
			ExitCode: code,
			Stdout:   stdout,
			Stderr:   stderr,
		}
		applied = append(applied, appliedStep{index: i, step: step})
	}
	return sr
}

// rollbackApplied runs the rollback command of each previously-applied step in
// REVERSE order. A step whose rollback command actually executes is marked
// "rolledback" and its rollback outcome is recorded separately in
// StepResult.Rollback, preserving the original apply result. A step with no
// rollback command is marked "rollback-skipped" (no undo was attempted) and its
// apply result is left untouched.
func rollbackApplied(exec ExecFn, server string, applied []appliedStep, steps []StepResult) {
	for k := len(applied) - 1; k >= 0; k-- {
		a := applied[k]
		if strings.TrimSpace(a.step.Rollback) == "" {
			steps[a.index].Status = StepRollbackSkipped
			continue
		}
		code, stdout, stderr, err := exec(server, a.step.Rollback)
		rb := &RollbackResult{
			ExitCode: code,
			Stdout:   stdout,
			Stderr:   stderr,
		}
		if err != nil {
			rb.Detail = err.Error()
		}
		steps[a.index].Rollback = rb
		steps[a.index].Status = StepRolledBack
	}
}

// ResolveTargets picks the set of servers a playbook should run against, given
// an optional explicit server, an optional --group tag expression, and the set
// of all known server names. Precedence: explicit server > group expression >
// the playbook's own Hosts expression. An empty result is an error.
//
// tags may be nil when no group/hosts expression needs resolving.
func ResolveTargets(pb Playbook, explicitServer, groupExpr string, allNames []string, tags *TagStore) ([]string, error) {
	if strings.TrimSpace(explicitServer) != "" {
		for _, name := range allNames {
			if name == explicitServer {
				return []string{explicitServer}, nil
			}
		}
		return nil, fmt.Errorf("unknown server %q", explicitServer)
	}

	expr := strings.TrimSpace(groupExpr)
	source := "--group"
	if expr == "" {
		expr = strings.TrimSpace(pb.Hosts)
		source = "playbook hosts"
	}
	if expr == "" {
		return nil, fmt.Errorf("no target specified: pass a server, use --group EXPR, or set 'hosts' in the playbook")
	}
	if tags == nil {
		return nil, fmt.Errorf("cannot resolve %s expression %q without a tag store", source, expr)
	}
	matched, err := tags.ServersMatching(expr, allNames)
	if err != nil {
		return nil, err
	}
	if len(matched) == 0 {
		return nil, fmt.Errorf("%s expression %q matched no servers", source, expr)
	}
	sort.Strings(matched)
	return matched, nil
}
