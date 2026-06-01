package workflow

import "strings"

// StageConfig carries the context needed to build a prompt for any stage type.
type StageConfig struct {
	Goal             string
	RepoURL          string
	Branch           string
	PreviousOutput   string // research output for plan; task description for implement
	ImplementSession string // for verify stages: the implement session whose workspace to check
	VerifierPrompt   string // empty = use default rubric; populated by special harness later
}

// ResearchPrompt builds the prompt for a research stage.
func ResearchPrompt(c StageConfig) string {
	return "You are a research agent. Investigate the goal below and provide structured findings " +
		"for a planning agent. Do not make any changes to any files.\n\n" +
		"Goal: " + c.Goal + "\n\n" +
		"Repository: " + c.RepoURL + " (branch: " + c.Branch + ")\n\n" +
		"Provide your findings under these exact headings:\n" +
		"## Goal\n" +
		"## Relevant Files\n" +
		"## Key Constraints\n" +
		"## Risks"
}

// PlanPrompt builds the prompt for a plan stage.
func PlanPrompt(c StageConfig) string {
	return "You are a planning agent. Based on the research findings below, create a concrete " +
		"implementation plan decomposed into independent tasks.\n\n" +
		"Goal: " + c.Goal + "\n\n" +
		"Research findings:\n" + c.PreviousOutput + "\n\n" +
		"Output ONLY a JSON array of task objects. No preamble, no explanation, no markdown fences.\n" +
		"Each object must have:\n" +
		"- \"id\": short unique identifier (e.g. \"task-1\")\n" +
		"- \"description\": clear, self-contained task that a coding agent can execute independently; " +
		"must name the specific files it touches\n\n" +
		"Example: [{\"id\":\"task-1\",\"description\":\"Add X to file Y\"}]"
}

// ImplementPrompt builds the prompt for an implement stage.
// c.PreviousOutput is the task description; c.Branch is the implement branch name.
func ImplementPrompt(c StageConfig) string {
	return "You are a coding agent. Complete ONLY the task described below. " +
		"Do not refactor unrelated code.\n\n" +
		"Task: " + c.PreviousOutput + "\n\n" +
		"Repository: " + c.RepoURL + " (base branch: " + c.Branch + ")\n\n" +
		"Create branch " + c.Branch + " from the base branch, implement the task, and push.\n\n" +
		"Before committing, run `go build ./...` and `go vet ./...`. " +
		"If either fails, fix the errors. Do not commit broken code.\n\n" +
		"End your output with this exact JSON block (fill in the values):\n" +
		`{"committed": true|false, "commit_sha": "<sha or empty>", "build_ok": true|false, "notes": "<optional notes>"}`
}

// VerifyPrompt builds the prompt for a verify stage.
// c.PreviousOutput is the original task description. c.ImplementSession is the
// implement session whose workspace is mounted at /home/claude/workspace/.
func VerifyPrompt(c StageConfig) string {
	var sb strings.Builder
	sb.WriteString("You are a code reviewer. You did NOT write this code.\n\n")
	sb.WriteString("The implementation you are reviewing is at /home/claude/workspace/.\n\n")
	sb.WriteString("Task that was supposed to be implemented:\n")
	sb.WriteString(c.PreviousOutput)
	sb.WriteString("\n\nVerification steps:\n")
	sb.WriteString("1. Run `go build ./...` — must succeed with no errors\n")
	sb.WriteString("2. Run `go vet ./...` — must produce no warnings\n")
	sb.WriteString("3. Confirm the task described above was actually completed as specified\n")
	sb.WriteString("4. Confirm no unrelated files were changed\n")
	sb.WriteString("5. Confirm a commit is present (`git log --oneline -1`)\n\n")

	if c.VerifierPrompt != "" {
		sb.WriteString("Additional criteria:\n")
		sb.WriteString(c.VerifierPrompt)
		sb.WriteString("\n\n")
	}

	sb.WriteString("Respond with ONLY this JSON object (no other text):\n")
	sb.WriteString(`{"pass": true|false, "issues": ["<issue 1>", "<issue 2>"]}`)
	sb.WriteString("\n\nIf pass is true, issues must be an empty array.")
	return sb.String()
}
