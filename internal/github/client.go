package github

import (
	"context"
	"net/http"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v72/github"
)

type GitHubClient struct {
	appID      int64
	privateKey []byte
}

func NewGitHubClient(appID int64, privateKey []byte) *GitHubClient {
	return &GitHubClient{appID: appID, privateKey: privateKey}
}

func (c *GitHubClient) GetInstallationAccessToken(ctx context.Context, installationID int64) (string, error) {
	itr, err := ghinstallation.New(http.DefaultTransport, c.appID, installationID, c.privateKey)
	if err != nil {
		return "", err
	}
	return itr.Token(ctx)
}

type CheckStatus string
type CheckConclusion string

const (
	CheckStatusInProgress CheckStatus = "in_progress"
	CheckStatusCompleted  CheckStatus = "completed"

	CheckConclusionSuccess CheckConclusion = "success"
	CheckConclusionFailure CheckConclusion = "failure"
)

func (c *GitHubClient) CreateCheckRun(ctx context.Context, installationID int64, owner, repo, sha, name string, status CheckStatus) (int64, error) {
	client, err := c.newClient(installationID)
	if err != nil {
		return 0, err
	}

	run, _, err := client.Checks.CreateCheckRun(ctx, owner, repo, gh.CreateCheckRunOptions{
		Name:    name,
		HeadSHA: sha,
		Status:  gh.Ptr(string(status)),
	})
	if err != nil {
		return 0, err
	}
	return run.GetID(), nil
}

func (c *GitHubClient) UpdateCheckRun(ctx context.Context, installationID int64, owner, repo string, checkRunID int64, conclusion CheckConclusion) error {
	client, err := c.newClient(installationID)
	if err != nil {
		return err
	}

	_, _, err = client.Checks.UpdateCheckRun(ctx, owner, repo, checkRunID, gh.UpdateCheckRunOptions{
		Name:       "deploy",
		Status:     gh.Ptr(string(CheckStatusCompleted)),
		Conclusion: gh.Ptr(string(conclusion)),
	})
	return err
}

func (c *GitHubClient) newClient(installationID int64) (*gh.Client, error) {
	itr, err := ghinstallation.New(http.DefaultTransport, c.appID, installationID, c.privateKey)
	if err != nil {
		return nil, err
	}
	return gh.NewClient(&http.Client{Transport: itr}), nil
}
