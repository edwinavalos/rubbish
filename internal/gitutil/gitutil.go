package gitutil

import "strings"

// RepoName extracts the repository name from a clone URL.
// e.g. "https://github.com/user/myrepo.git" → "myrepo"
func RepoName(url string) string {
	url = strings.TrimRight(url, "/")
	url = strings.TrimSuffix(url, ".git")
	idx := strings.LastIndexAny(url, "/:")
	if idx < 0 || idx == len(url)-1 {
		return ""
	}
	return url[idx+1:]
}
