package workflow

import (
	"strings"
	"testing"
)

// --- ValidatePlanOutput ---

func TestValidatePlanOutput_validJSON(t *testing.T) {
	tasks, err := ValidatePlanOutput(`[{"id":"task-1","description":"Add X to file Y"}]`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].ID != "task-1" {
		t.Errorf("task ID = %q, want %q", tasks[0].ID, "task-1")
	}
	if tasks[0].Description != "Add X to file Y" {
		t.Errorf("task description = %q, want %q", tasks[0].Description, "Add X to file Y")
	}
}

func TestValidatePlanOutput_invalidJSON(t *testing.T) {
	_, err := ValidatePlanOutput("not json at all")
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "not valid JSON")
	}
}

func TestValidatePlanOutput_emptyArray(t *testing.T) {
	_, err := ValidatePlanOutput(`[]`)
	if err == nil {
		t.Fatal("expected error for empty array, got nil")
	}
	if !strings.Contains(err.Error(), "empty task list") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "empty task list")
	}
}

func TestValidatePlanOutput_missingID(t *testing.T) {
	_, err := ValidatePlanOutput(`[{"description":"Do something"}]`)
	if err == nil {
		t.Fatal("expected error for missing id, got nil")
	}
	if !strings.Contains(err.Error(), "missing id") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "missing id")
	}
}

func TestValidatePlanOutput_missingDescription(t *testing.T) {
	_, err := ValidatePlanOutput(`[{"id":"task-1"}]`)
	if err == nil {
		t.Fatal("expected error for missing description, got nil")
	}
	if !strings.Contains(err.Error(), "missing description") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "missing description")
	}
}

func TestValidatePlanOutput_extraFieldsOK(t *testing.T) {
	tasks, err := ValidatePlanOutput(`[{"id":"t1","description":"Do X","priority":"high","files":["foo.go"]}]`)
	if err != nil {
		t.Fatalf("unexpected error with extra fields: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(tasks))
	}
}

func TestValidatePlanOutput_multipleTasks(t *testing.T) {
	tasks, err := ValidatePlanOutput(`[
		{"id":"t1","description":"First task"},
		{"id":"t2","description":"Second task"}
	]`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestValidatePlanOutput_proseWrapped(t *testing.T) {
	// Plan output may contain prose before/after the JSON array.
	output := "Here is the plan:\n[{\"id\":\"t1\",\"description\":\"Do X\"}]\nDone."
	tasks, err := ValidatePlanOutput(output)
	if err != nil {
		t.Fatalf("unexpected error for prose-wrapped JSON: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(tasks))
	}
}

// --- ValidateImplementOutput ---

func goodTerminalBlock() string {
	return `{"committed":true,"commit_sha":"abc123def456","build_ok":true,"notes":""}`
}

func TestValidateImplementOutput_allGood(t *testing.T) {
	ok, issues := ValidateImplementOutput(goodTerminalBlock())
	if !ok {
		t.Errorf("expected ok=true, got false; issues=%v", issues)
	}
	if len(issues) != 0 {
		t.Errorf("expected no issues, got %v", issues)
	}
}

func TestValidateImplementOutput_noTerminalBlock(t *testing.T) {
	ok, issues := ValidateImplementOutput("I completed the task. No structured output.")
	if ok {
		t.Error("expected ok=false for missing terminal block")
	}
	if !containsAny(issues, "no terminal status block") {
		t.Errorf("issues %v should contain %q", issues, "no terminal status block")
	}
}

func TestValidateImplementOutput_notCommitted(t *testing.T) {
	output := `{"committed":false,"commit_sha":"","build_ok":true,"notes":"nothing to commit"}`
	ok, issues := ValidateImplementOutput(output)
	if ok {
		t.Error("expected ok=false when not committed")
	}
	if !containsAny(issues, "agent did not commit") {
		t.Errorf("issues %v should contain %q", issues, "agent did not commit")
	}
}

func TestValidateImplementOutput_buildFailed(t *testing.T) {
	output := `{"committed":true,"commit_sha":"abc123","build_ok":false,"notes":"undefined: Foo"}`
	ok, issues := ValidateImplementOutput(output)
	if ok {
		t.Error("expected ok=false when build failed")
	}
	if !containsAny(issues, "build failed") {
		t.Errorf("issues %v should contain %q", issues, "build failed")
	}
}

func TestValidateImplementOutput_missingCommitSHA(t *testing.T) {
	output := `{"committed":true,"commit_sha":"","build_ok":true,"notes":""}`
	ok, issues := ValidateImplementOutput(output)
	if ok {
		t.Error("expected ok=false when commit sha is empty")
	}
	if !containsAny(issues, "commit sha missing") {
		t.Errorf("issues %v should contain %q", issues, "commit sha missing")
	}
}

func TestValidateImplementOutput_blockEmbeddedInText(t *testing.T) {
	output := "I completed the implementation.\n\nHere is the summary:\n" +
		`{"committed":true,"commit_sha":"abc123def456","build_ok":true,"notes":""}` +
		"\n\nAll tests pass."
	ok, issues := ValidateImplementOutput(output)
	if !ok {
		t.Errorf("expected ok=true for embedded terminal block; issues=%v", issues)
	}
}

func TestValidateImplementOutput_multipleIssues(t *testing.T) {
	output := `{"committed":false,"commit_sha":"","build_ok":false,"notes":""}`
	ok, issues := ValidateImplementOutput(output)
	if ok {
		t.Error("expected ok=false for multiple failures")
	}
	if len(issues) < 2 {
		t.Errorf("expected at least 2 issues, got %d: %v", len(issues), issues)
	}
}

// containsAny reports whether any element of issues contains the substring sub.
func containsAny(issues []string, sub string) bool {
	for _, s := range issues {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
