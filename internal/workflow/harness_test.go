package workflow

import (
	"strings"
	"testing"
)

func cfg(goal string) StageConfig {
	return StageConfig{
		Goal:    goal,
		RepoURL: "https://github.com/test/repo.git",
		Branch:  "main",
	}
}

// --- ResearchPrompt ---

func TestResearchPrompt_containsGoal(t *testing.T) {
	c := cfg("migrate logging to slog")
	p := ResearchPrompt(c)
	if !strings.Contains(p, "migrate logging to slog") {
		t.Errorf("ResearchPrompt missing goal; prompt = %q", p)
	}
}

func TestResearchPrompt_containsReadOnly(t *testing.T) {
	p := ResearchPrompt(cfg("some goal"))
	if !strings.Contains(p, "Do not make any changes") {
		t.Errorf("ResearchPrompt missing read-only instruction; prompt = %q", p)
	}
}

func TestResearchPrompt_containsOutputHeadings(t *testing.T) {
	p := ResearchPrompt(cfg("some goal"))
	for _, heading := range []string{"## Relevant Files", "## Key Constraints", "## Risks"} {
		if !strings.Contains(p, heading) {
			t.Errorf("ResearchPrompt missing heading %q; prompt = %q", heading, p)
		}
	}
}

func TestResearchPrompt_containsRepoURL(t *testing.T) {
	c := cfg("some goal")
	c.RepoURL = "https://github.com/myorg/myrepo.git"
	p := ResearchPrompt(c)
	if !strings.Contains(p, "myorg/myrepo") {
		t.Errorf("ResearchPrompt missing repo URL; prompt = %q", p)
	}
}

// --- PlanPrompt ---

func TestPlanPrompt_containsGoal(t *testing.T) {
	c := cfg("add rate limiting")
	p := PlanPrompt(c)
	if !strings.Contains(p, "add rate limiting") {
		t.Errorf("PlanPrompt missing goal; prompt = %q", p)
	}
}

func TestPlanPrompt_containsResearchOutput(t *testing.T) {
	c := cfg("add rate limiting")
	c.PreviousOutput = "## Relevant Files\nengine.go is the main file"
	p := PlanPrompt(c)
	if !strings.Contains(p, "engine.go is the main file") {
		t.Errorf("PlanPrompt missing research output; prompt = %q", p)
	}
}

func TestPlanPrompt_requiresJSONOnly(t *testing.T) {
	p := PlanPrompt(cfg("some goal"))
	if !strings.Contains(p, "ONLY a JSON array") {
		t.Errorf("PlanPrompt missing JSON-only instruction; prompt = %q", p)
	}
}

func TestPlanPrompt_requiresFileNames(t *testing.T) {
	p := PlanPrompt(cfg("some goal"))
	if !strings.Contains(p, "name the specific files") {
		t.Errorf("PlanPrompt missing file-naming instruction; prompt = %q", p)
	}
}

// --- ImplementPrompt ---

func TestImplementPrompt_containsTaskDescription(t *testing.T) {
	c := cfg("some goal")
	c.PreviousOutput = "Refactor the auth module in internal/auth/handler.go"
	p := ImplementPrompt(c)
	if !strings.Contains(p, "internal/auth/handler.go") {
		t.Errorf("ImplementPrompt missing task description; prompt = %q", p)
	}
}

func TestImplementPrompt_containsAntiDrift(t *testing.T) {
	p := ImplementPrompt(cfg("some goal"))
	if !strings.Contains(p, "Do not refactor unrelated code") {
		t.Errorf("ImplementPrompt missing anti-drift instruction; prompt = %q", p)
	}
}

func TestImplementPrompt_requiresBuildCheck(t *testing.T) {
	p := ImplementPrompt(cfg("some goal"))
	if !strings.Contains(p, "go build ./...") {
		t.Errorf("ImplementPrompt missing go build instruction; prompt = %q", p)
	}
}

func TestImplementPrompt_requiresTerminalBlock(t *testing.T) {
	p := ImplementPrompt(cfg("some goal"))
	if !strings.Contains(p, `"committed"`) {
		t.Errorf("ImplementPrompt missing terminal block schema; prompt = %q", p)
	}
}

func TestImplementPrompt_containsBranchName(t *testing.T) {
	c := cfg("some goal")
	c.Branch = "implement/abc12345-0"
	p := ImplementPrompt(c)
	if !strings.Contains(p, "implement/abc12345-0") {
		t.Errorf("ImplementPrompt missing branch name; prompt = %q", p)
	}
}

// --- VerifyPrompt ---

func TestVerifyPrompt_containsWorkspacePath(t *testing.T) {
	c := cfg("some goal")
	c.ImplementSession = "session-abc123"
	p := VerifyPrompt(c)
	if !strings.Contains(p, "/home/claude/workspace/") {
		t.Errorf("VerifyPrompt missing workspace path; prompt = %q", p)
	}
}

func TestVerifyPrompt_containsBuildInstructions(t *testing.T) {
	c := cfg("some goal")
	c.ImplementSession = "session-abc123"
	p := VerifyPrompt(c)
	if !strings.Contains(p, "go build ./...") {
		t.Errorf("VerifyPrompt missing go build instruction; prompt = %q", p)
	}
}

func TestVerifyPrompt_containsTaskDescription(t *testing.T) {
	c := cfg("some goal")
	c.PreviousOutput = "Add rate limiting to the API handler"
	c.ImplementSession = "session-abc123"
	p := VerifyPrompt(c)
	if !strings.Contains(p, "Add rate limiting to the API handler") {
		t.Errorf("VerifyPrompt missing task description; prompt = %q", p)
	}
}

func TestVerifyPrompt_requiresPassJSON(t *testing.T) {
	c := cfg("some goal")
	c.ImplementSession = "session-abc123"
	p := VerifyPrompt(c)
	if !strings.Contains(p, `"pass"`) {
		t.Errorf("VerifyPrompt missing pass JSON schema; prompt = %q", p)
	}
}

func TestVerifyPrompt_customVerifierPromptAppended(t *testing.T) {
	c := cfg("some goal")
	c.ImplementSession = "session-abc123"
	c.VerifierPrompt = "also verify that error handling follows project conventions"
	p := VerifyPrompt(c)
	if !strings.Contains(p, "also verify that error handling follows project conventions") {
		t.Errorf("VerifyPrompt missing custom verifier prompt; prompt = %q", p)
	}
}

func TestVerifyPrompt_defaultWhenNoCustomPrompt(t *testing.T) {
	c := cfg("some goal")
	c.ImplementSession = "session-abc123"
	c.VerifierPrompt = "" // empty = use default
	p := VerifyPrompt(c)
	// Should still have the default rubric content
	if !strings.Contains(p, "go vet") {
		t.Errorf("VerifyPrompt missing default rubric (go vet); prompt = %q", p)
	}
}
