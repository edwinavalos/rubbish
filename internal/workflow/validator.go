package workflow

import (
	"encoding/json"
	"fmt"
	"regexp"
)

var terminalBlockRe = regexp.MustCompile(`(?s)\{[^{}]*"committed"[^{}]*\}`)

type terminalBlock struct {
	Committed bool   `json:"committed"`
	CommitSHA string `json:"commit_sha"`
	BuildOK   bool   `json:"build_ok"`
	Notes     string `json:"notes"`
}

// ValidatePlanOutput parses and validates a JSON task array from the plan stage.
// It tolerates leading/trailing prose. Returns a descriptive error if the output
// is structurally invalid.
func ValidatePlanOutput(output string) ([]Task, error) {
	loc := jsonArrayRe.FindStringIndex(output)
	if loc == nil {
		return nil, fmt.Errorf("plan output is not valid JSON: no JSON array found")
	}
	raw := output[loc[0]:loc[1]]

	var tasks []Task
	if err := json.Unmarshal([]byte(raw), &tasks); err != nil {
		return nil, fmt.Errorf("plan output is not valid JSON: %w", err)
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("plan returned empty task list")
	}
	for i, task := range tasks {
		if task.ID == "" {
			return nil, fmt.Errorf("task %d missing id", i)
		}
		if task.Description == "" {
			return nil, fmt.Errorf("task %d missing description", i)
		}
	}
	return tasks, nil
}

// ValidateImplementOutput checks the implement stage output for the required
// terminal JSON block and validates its fields. Returns (true, nil) on success.
// Returns (false, issues) listing every specific failure found.
func ValidateImplementOutput(output string) (bool, []string) {
	loc := terminalBlockRe.FindStringIndex(output)
	if loc == nil {
		return false, []string{"no terminal status block in output"}
	}

	var block terminalBlock
	if err := json.Unmarshal([]byte(output[loc[0]:loc[1]]), &block); err != nil {
		return false, []string{"no terminal status block in output"}
	}

	var issues []string
	if !block.Committed {
		issues = append(issues, "agent did not commit")
	}
	if block.CommitSHA == "" {
		issues = append(issues, "commit sha missing")
	}
	if !block.BuildOK {
		issues = append(issues, "build failed per agent report")
	}
	return len(issues) == 0, issues
}
