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

// Markdownのテーブルを作成する関数
func BuildMarkdownTable(headers []string, rows [][]string) string {
	var sb strings.Builder

	sb.WriteString("|")
	for _, header := range headers {
		sb.WriteString(" " + header + " |")
	}
	sb.WriteString("\n")

	sb.WriteString("|")
	for range headers {
		sb.WriteString(" --- |")
	}
	sb.WriteString("\n")

	for _, row := range rows {
		sb.WriteString("|")
		for _, cell := range row {
			sb.WriteString(" " + cell + " |")
		}
		sb.WriteString("\n")
	}

	return sb.String()
}
