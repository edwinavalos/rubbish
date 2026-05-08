package gitutil_test

import (
	"testing"

	"github.com/edwinavalos/rubbish/internal/gitutil"
)

func TestRepoName(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://github.com/edwinavalos/rubbish", "rubbish"},
		{"https://github.com/edwinavalos/rubbish.git", "rubbish"},
		{"https://github.com/edwinavalos/rubbish/", "rubbish"},
		{"git@github.com:edwinavalos/rubbish.git", "rubbish"},
		{"", ""},
	}
	for _, tc := range cases {
		got := gitutil.RepoName(tc.url)
		if got != tc.want {
			t.Errorf("RepoName(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}
