package config

import "fmt"

const (
	SnappyAppSecretName = "snappy-app-secret"
	SnappyAppSecretNS   = "snappy-system"
	InstallationIDKey = "installation-id"

	InstallationAccessTokenKey = "installation-access-token"

	LastPushAnnotation = "snappy/last-push-sha"
	CheckRunAnnotation = "snappy/check-run-id"
)

func BuildPushImageJobName(appName, sha string) string {
	return fmt.Sprintf("%s-build-push-%s", appName, sha[:8])
}

func BuildImageJobName(appName, sha string) string {
	return fmt.Sprintf("%s-build-%s", appName, sha[:8])
}

func RepoSecretName(appName string) string {
	return fmt.Sprintf("%s-repo-auth", appName)
}
