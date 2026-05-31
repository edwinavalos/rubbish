package workflow

import "testing"

func TestIsUsageLimitError(t *testing.T) {
	trueCases := []string{
		"You've hit your org's monthly usage limit",
		"You've hit your weekly limit · resets Mon 12:00am",
		"You've hit your session limit · resets 3:45pm",
		"You've hit your Opus limit · resets tomorrow",
		// case-insensitive lower variant
		"you've hit your monthly limit",
	}
	for _, s := range trueCases {
		if !IsUsageLimitError(s) {
			t.Errorf("IsUsageLimitError(%q) = false, want true", s)
		}
	}

	falseCases := []string{
		"",
		"Task complete. All files updated.",
		// Contains "usage" and "limit" but not the Claude-specific phrase
		"I updated the usage limit handler in engine.go",
		"The rate limit for this endpoint is 100 req/s",
		"Build failed: exit status 1",
	}
	for _, s := range falseCases {
		if IsUsageLimitError(s) {
			t.Errorf("IsUsageLimitError(%q) = true, want false", s)
		}
	}
}
