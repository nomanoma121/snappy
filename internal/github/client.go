package github

import (
	"context"
	"log"
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

type CreateCheckRunOptions struct {
	Name    string
	HeadSHA string
	Status  CheckStatus
	Title   string
	Summary string
	Text    string
}

type CreateCheckRunParams struct {
	InstallationID        int64
	Owner                 string
	Repo                  string
	CreateCheckRunOptions CreateCheckRunOptions
}

func (c *GitHubClient) CreateCheckRun(ctx context.Context, params CreateCheckRunParams) (int64, error) {
	client, err := c.newClient(params.InstallationID)
	if err != nil {
		return 0, err
	}

	run, _, err := client.Checks.CreateCheckRun(ctx, params.Owner, params.Repo, gh.CreateCheckRunOptions{
		Name:    params.CreateCheckRunOptions.Name,
		HeadSHA: params.CreateCheckRunOptions.HeadSHA,
		Status:  gh.Ptr(string(params.CreateCheckRunOptions.Status)),
		Output: &gh.CheckRunOutput{
			Title:   gh.Ptr(params.CreateCheckRunOptions.Title),
			Summary: gh.Ptr(params.CreateCheckRunOptions.Summary),
			Text:    gh.Ptr(params.CreateCheckRunOptions.Text),
		},
	})
	if err != nil {
		return 0, err
	}
	log.Printf("created check run with ID: %d", run.GetID())
	return run.GetID(), nil
}

type UpdateCheckRunOptions struct {
	Name       string
	Status     CheckStatus
	Conclusion CheckConclusion
	Title      string
	Summary    string
	Text       string
}

type UpdateCheckRunParams struct {
	InstallationID        int64
	Owner                 string
	Repo                  string
	CheckRunID            int64
	UpdateCheckRunOptions UpdateCheckRunOptions
}

func (c *GitHubClient) UpdateCheckRun(ctx context.Context, params UpdateCheckRunParams) error {
	client, err := c.newClient(params.InstallationID)
	if err != nil {
		return err
	}

	_, _, err = client.Checks.UpdateCheckRun(ctx, params.Owner, params.Repo, params.CheckRunID, gh.UpdateCheckRunOptions{
		Name:       params.UpdateCheckRunOptions.Name,
		Status:     gh.Ptr(string(params.UpdateCheckRunOptions.Status)),
		Conclusion: gh.Ptr(string(params.UpdateCheckRunOptions.Conclusion)),
		Output: &gh.CheckRunOutput{
			Title:   gh.Ptr(params.UpdateCheckRunOptions.Title),
			Summary: gh.Ptr(params.UpdateCheckRunOptions.Summary),
			Text:    gh.Ptr(params.UpdateCheckRunOptions.Text),
		},
	})
	log.Printf("updated check run with ID: %d, conclusion: %s", checkRunID, conclusion)
	return err
}

func (c *GitHubClient) newClient(installationID int64) (*gh.Client, error) {
	itr, err := ghinstallation.New(http.DefaultTransport, c.appID, installationID, c.privateKey)
	if err != nil {
		return nil, err
	}
	return gh.NewClient(&http.Client{Transport: itr}), nil
}
