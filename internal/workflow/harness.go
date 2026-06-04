package workflow

import "strings"

// StageConfig carries the context needed to build a prompt for any stage type.
type StageConfig struct {
	Goal             string
	RepoURL          string
	Branch           string // for implement: the implement branch name; for verify: same
	BaseBranch       string // for implement: the branch to create the implement branch from
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
// c.Branch is the implement branch name; c.BaseBranch is the branch to fork from.
func ImplementPrompt(c StageConfig) string {
	base := c.BaseBranch
	if base == "" {
		base = "main"
	}
	return "You are a coding agent. Complete ONLY the task described below. " +
		"Do not refactor unrelated code.\n\n" +
		"Task: " + c.PreviousOutput + "\n\n" +
		"Repository: " + c.RepoURL + "\n\n" +
		"Steps:\n" +
		"1. Clone the repository and check out base branch `" + base + "`\n" +
		"2. Create and check out branch `" + c.Branch + "` from `" + base + "`\n" +
		"3. Implement the task\n" +
		"4. Run `go build ./...` and `go vet ./...` — fix any errors before committing\n" +
		"5. Commit your changes with a descriptive message\n" +
		"6. Push branch `" + c.Branch + "` to origin\n\n" +
		"End your output with this exact JSON block (fill in the values):\n" +
		`{"committed": true|false, "commit_sha": "<sha or empty>", "build_ok": true|false, "notes": "<optional notes>"}`
}

// VerifyPrompt builds the prompt for a verify stage.
// c.Branch is the implement branch to fetch and check out.
// c.PreviousOutput is the original task description.
func VerifyPrompt(c StageConfig) string {
	var sb strings.Builder
	sb.WriteString("You are a code reviewer. You did NOT write this code.\n\n")
	sb.WriteString("The repository is cloned at /home/claude/workspace/.\n")
	if c.Branch != "" {
		sb.WriteString("First, fetch and check out the implement branch:\n")
		sb.WriteString("  cd /home/claude/workspace && git fetch origin " + c.Branch + " && git checkout " + c.Branch + "\n\n")
	}
	sb.WriteString("Task that was supposed to be implemented:\n")
	sb.WriteString(c.PreviousOutput)
	sb.WriteString("\n\nVerification steps:\n")
	sb.WriteString("1. Run `go build ./...` from /home/claude/workspace — must succeed with no errors\n")
	sb.WriteString("2. Run `go vet ./...` — must produce no warnings\n")
	sb.WriteString("3. Confirm the task described above was actually completed as specified\n")
	sb.WriteString("4. Confirm no unrelated files were changed\n")
	sb.WriteString("5. Confirm a new commit is present on this branch (`git log --oneline -3`)\n\n")

	if c.VerifierPrompt != "" {
		sb.WriteString("Additional criteria:\n")
		sb.WriteString(c.VerifierPrompt)
		sb.WriteString("\n\n")
	}

	sb.WriteString("Your entire response MUST be exactly this JSON object and nothing else — no prose, no markdown, no explanation before or after:\n")
	sb.WriteString(`{"pass": true|false, "issues": ["<issue 1>", "<issue 2>"]}`)
	sb.WriteString("\n\nRules:\n")
	sb.WriteString("- Output the JSON object as your first and only content.\n")
	sb.WriteString("- If pass is true, issues MUST be an empty array [].\n")
	sb.WriteString("- Do not wrap the JSON in a code block or add any surrounding text.\n")
	sb.WriteString("- MACHINE-READABLE OUTPUT ONLY.")
	return sb.String()
}
