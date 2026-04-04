package github

import (
	"strings"
)

// ParseRepoURL extracts owner and repo from a GitHub URL.
// e.g. "https://github.com/owner/repo" → ("owner", "repo")
func ParseRepoURL(repoURL string) (owner, repo string) {
	repoURL = strings.TrimSuffix(repoURL, ".git")
	parts := strings.Split(repoURL, "/")
	if len(parts) < 2 {
		return "", ""
	}
	return parts[len(parts)-2], parts[len(parts)-1]
}