package main

import "testing"

func TestParseClaudeJSON(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		result  string
		isError bool
	}{
		{
			name:    "success result",
			input:   `{"type":"result","subtype":"success","result":"All done.","is_error":false}`,
			result:  "All done.",
			isError: false,
		},
		{
			name:    "error result",
			input:   `{"type":"result","subtype":"error_during_execution","result":"something went wrong","is_error":true}`,
			result:  "something went wrong",
			isError: true,
		},
		{
			name:    "empty result field",
			input:   `{"type":"result","is_error":false,"result":""}`,
			result:  "",
			isError: false,
		},
		{
			name:    "not json — raw text fallback",
			input:   "plain text output from agent",
			result:  "plain text output from agent",
			isError: false,
		},
		{
			name:    "empty string",
			input:   "",
			result:  "",
			isError: false,
		},
		{
			name:    "whitespace-only",
			input:   "   \n  ",
			result:  "",
			isError: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotResult, gotIsError := parseClaudeJSON(c.input)
			if gotResult != c.result {
				t.Errorf("result = %q, want %q", gotResult, c.result)
			}
			if gotIsError != c.isError {
				t.Errorf("isError = %v, want %v", gotIsError, c.isError)
			}
		})
	}
}
