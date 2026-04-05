package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	gh "github.com/google/go-github/v72/github"
	appsv1alpha1 "github.com/nomanoma121/snappy/api/v1alpha1"
	"github.com/nomanoma121/snappy/internal/config"
	"github.com/nomanoma121/snappy/internal/github"
	"github.com/nomanoma121/snappy/internal/resource"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	githubAppName = "snappy-release"

	checkRunName = "Build Image"
)

var interestedActions = []string{"opened", "synchronize"}

type Server struct {
	router chi.Router
	addr   string
	github *github.GitHubClient
	k8s    client.Client
}

func NewServer(addr string, gh *github.GitHubClient, k8s client.Client) *Server {
	s := &Server{addr: addr, github: gh, k8s: k8s}

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/github/install", s.Install)
	r.Get("/github/callback", s.Callback)
	r.Post("/github/webhook", s.Webhook)

	s.router = r
	return s
}

func (s *Server) Start() error {
	return http.ListenAndServe(s.addr, s.router)
}

func (s *Server) Install(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r,
		fmt.Sprintf("https://github.com/apps/%s/installations/new", githubAppName),
		http.StatusFound)
}

func (s *Server) Callback(w http.ResponseWriter, r *http.Request) {
	installationIDStr := r.URL.Query().Get("installation_id")
	if installationIDStr == "" {
		http.Error(w, "installation_id is required", http.StatusBadRequest)
		return
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.SnappyAppSecretName,
			Namespace: config.SnappyAppSecretNS,
		},
		Data: map[string][]byte{
			config.InstallationIDKey: []byte(installationIDStr),
		},
	}
	if err := s.k8s.Create(r.Context(), secret); err != nil {
		log.Printf("failed to save installation_id: %v", err)
		http.Error(w, "failed to save installation_id", http.StatusInternalServerError)
		return
	}

	// TODO: ここをちょっとだけリッチにする
	w.Write([]byte("GitHub App installed successfully"))
}

func (s *Server) Webhook(w http.ResponseWriter, r *http.Request) {
	event := r.Header.Get("X-GitHub-Event")
	ctx := r.Context()
	switch event {
	case "push":
		s.handlePushEvent(ctx, w, r)
	case "pull_request":
		s.handlePullRequestEvent(ctx, w, r)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handlePushEvent(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var payload gh.PushEvent
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		log.Printf("failed to decode payload: %v", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	branch := branchFromRef(*payload.Ref)
	appList := &appsv1alpha1.AppList{}
	if err := s.k8s.List(ctx, appList); err != nil {
		log.Printf("failed to list apps: %v", err)
		http.Error(w, "failed to list apps", http.StatusInternalServerError)
		return
	}

	for _, app := range appList.Items {
		if compareRepositoryURL(app.Spec.Source.Repo, *payload.Repo.CloneURL) && app.Spec.Source.Branch == branch {
			// Annotaiionを更新してReconcileを走らせる
			if err := s.updateLastPushSHA(ctx, &app, *payload.After); err != nil {
				log.Printf("failed to update app: %v", err)
				http.Error(w, "failed to update app", http.StatusInternalServerError)
				return
			}
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

type pullRequestEventContext struct {
	app            *appsv1alpha1.App
	job            *batchv1.Job
	owner          string
	repo           string
	sha            string
	prNumber       int
	installationID int64
	checkRunID     int64
	commentID      int64
}

func (s *Server) handlePullRequestEvent(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var payload gh.PullRequestEvent
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		log.Printf("failed to decode payload: %v", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	// Closedのような関係の無いイベントは無視する
	action := payload.GetAction()
	if !slices.Contains(interestedActions, action) {
		log.Printf("ignoring pull request event with action: %s", action)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	prCtx := pullRequestEventContext{
		owner:    payload.GetRepo().GetOwner().GetLogin(),
		repo:     payload.GetRepo().GetName(),
		sha:      payload.GetPullRequest().GetHead().GetSHA(),
		prNumber: payload.GetNumber(),
	}

	var err error
	prCtx.app, err = s.lookupApp(ctx, payload.GetRepo().GetCloneURL(), branchFromRef(payload.GetPullRequest().GetBase().GetRef()))
	if err != nil {
		log.Printf("failed to lookup app: %v", err)
		http.Error(w, "app not found", http.StatusNotFound)
		return
	}
	prCtx.installationID, err = s.getInstallationID(ctx)
	if err != nil {
		log.Printf("failed to get installation ID: %v", err)
		http.Error(w, "failed to get installation ID", http.StatusInternalServerError)
		return
	}

	if prCtx.checkRunID, err = s.notifyBuildStatus(ctx, &prCtx, notifyBuildStatusParams{
			title: fmt.Sprintf("Building image for %s...", prCtx.app.Name),
			summary: github.BuildMarkdownTable(
				[]string{"Name", "Latest Commit", "Status"},
				[][]string{{prCtx.app.Name, prCtx.sha[:8], "In Progress"}},
			),
			status: github.CheckStatusInProgress,
	}); err != nil {
		log.Printf("failed to notify build status: %v", err)
		http.Error(w, "failed to notify build status", http.StatusInternalServerError)
		return
	}

	prCtx.job = resource.NewBuildImageJob(prCtx.app, config.BuildImageJobName(prCtx.app.Name, prCtx.sha), prCtx.sha, config.RepoSecretName(prCtx.app.Name))
	if err := s.k8s.Create(ctx, prCtx.job); err != nil {
		log.Printf("failed to create job: %v", err)
		http.Error(w, "failed to create job", http.StatusInternalServerError)
		return
	}

	go s.watchJob(&prCtx, s.onJobComplete)

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) watchJob(prCtx *pullRequestEventContext, fn func(ctx context.Context, prCtx *pullRequestEventContext, succeeded bool)) {
	ctx := context.Background()
	for {
		var job batchv1.Job
		if err := s.k8s.Get(context.Background(), client.ObjectKey{Name: prCtx.job.Name, Namespace: prCtx.job.Namespace}, &job); err != nil {
			log.Printf("failed to get job: %v", err)
			return
		}
		if job.Status.Succeeded > 0 {
			fn(ctx, prCtx, true)
			return
		}
		if job.Status.Failed > 0 {
			fn(ctx, prCtx, false)
			return
		}
		time.Sleep(1 * time.Second)
	}
}

type notifyBuildStatusParams struct {
	status     github.CheckStatus
	conclusion github.CheckConclusion
	title      string
	summary    string
	text       string
}

func (s *Server) notifyBuildStatus(ctx context.Context, prCtx *pullRequestEventContext, params notifyBuildStatusParams) (int64, error) {
	_, err := s.createOrUpdateIssueComment(ctx, prCtx.app, createOrUpdateIssueCommentParams{
		installationID: prCtx.installationID,
		owner:          prCtx.owner,
		repo:           prCtx.repo,
		prNumber:       prCtx.prNumber,
		comment:        params.summary,
	})
	if err != nil {
		log.Printf("failed to create or update issue comment: %v", err)
		return 0, err
	}

	checkRunID, err := s.github.CreateCheckRun(ctx, github.CreateCheckRunParams{
		InstallationID: prCtx.installationID,
		Owner:          prCtx.owner,
		Repo:           prCtx.repo,
		CreateCheckRunOptions: github.CreateCheckRunOptions{
			Name:    checkRunName,
			HeadSHA: prCtx.sha,
			Status:  params.status,
			Title:   params.title,
			Summary: params.summary,
			Text:    params.text,
		},
	})
	if err != nil {
		log.Printf("failed to create check run: %v", err)
		return 0, err
	}
	return checkRunID, nil
}

func(s *Server) onJobComplete(ctx context.Context, prCtx *pullRequestEventContext, succeeded bool) {
	var notifyBuildStatusParams notifyBuildStatusParams
	if succeeded {
		notifyBuildStatusParams.conclusion = github.CheckConclusionSuccess
		notifyBuildStatusParams.title = "✅ Built Successfully"
		notifyBuildStatusParams.summary = fmt.Sprintf("The image for %s has been built successfully.", prCtx.app.Name)
		notifyBuildStatusParams.text = fmt.Sprintf("The image for %s has been built successfully. The deployment will start shortly.", prCtx.app.Name)
	} else {
		notifyBuildStatusParams.conclusion = github.CheckConclusionFailure
		notifyBuildStatusParams.title = "❌ Build Failed"
		notifyBuildStatusParams.summary = fmt.Sprintf("The image for %s failed to build.", prCtx.app.Name)
		notifyBuildStatusParams.text = fmt.Sprintf("The image for %s failed to build.", prCtx.app.Name)
	}
	if _, err := s.notifyBuildStatus(ctx, prCtx, notifyBuildStatusParams); err != nil {
		log.Printf("failed to notify build status: %v", err)
	}
}

func (s *Server) lookupApp(ctx context.Context, repoURL, branch string) (*appsv1alpha1.App, error) {
	appList := &appsv1alpha1.AppList{}
	if err := s.k8s.List(ctx, appList); err != nil {
		log.Printf("failed to list apps: %v", err)
		return nil, fmt.Errorf("failed to list apps: %w", err)
	}

	var targetApp *appsv1alpha1.App
	for _, app := range appList.Items {
		if compareRepositoryURL(app.Spec.Source.Repo, repoURL) && app.Spec.Source.Branch == branch {
			targetApp = &app
			break
		}
	}
	if targetApp == nil {
		log.Printf("app not found for repo %s and branch %s", repoURL, branch)
		return nil, fmt.Errorf("app not found for repo %s and branch %s", repoURL, branch)
	}

	return targetApp, nil
}

func (s *Server) getInstallationID(ctx context.Context) (int64, error) {
	var secret corev1.Secret
	if err := s.k8s.Get(ctx, client.ObjectKey{Name: config.SnappyAppSecretName, Namespace: config.SnappyAppSecretNS}, &secret); err != nil {
		log.Printf("failed to get installation ID secret: %v", err)
		return 0, fmt.Errorf("failed to get installation ID secret: %w", err)
	}
	return strconv.ParseInt(string(secret.Data[config.InstallationIDKey]), 10, 64)
}

func (s *Server) getCommentID(app *appsv1alpha1.App) (int64, bool, error) {
	if app.Annotations == nil {
		return 0, false, nil
	}
	commentIDStr, ok := app.Annotations[config.CheckRunAnnotation]
	if !ok {
		return 0, false, nil
	}
	commentID, err := strconv.ParseInt(commentIDStr, 10, 64)
	if err != nil {
		log.Printf("invalid comment ID: %v", err)
		return 0, false, fmt.Errorf("invalid comment ID: %w", err)
	}
	return commentID, true, nil
}

func (s *Server) setCommentID(ctx context.Context, app *appsv1alpha1.App, commentID int64) error {
	if app.Annotations == nil {
		app.Annotations = map[string]string{}
	}
	app.Annotations[config.CheckRunAnnotation] = strconv.FormatInt(commentID, 10)
	return s.k8s.Update(ctx, app)
}

type createOrUpdateIssueCommentParams struct {
	installationID int64
	owner          string
	repo           string
	prNumber       int
	comment        string
}

func (s *Server) createOrUpdateIssueComment(ctx context.Context, app *appsv1alpha1.App, params createOrUpdateIssueCommentParams) (int64, error) {
	commentID, exist, err := s.getCommentID(app)
	if err != nil {
		return 0, fmt.Errorf("failed to get comment ID: %w", err)
	}
	if exist {
		if err := s.github.UpdateIssueComment(ctx, github.UpdateIssueCommentParams{
			InstallationID: params.installationID,
			Owner:          params.owner,
			Repo:           params.repo,
			CommentID:      commentID,
			Comment:        params.comment,
		}); err != nil {
			return commentID, fmt.Errorf("failed to update issue comment: %w", err)
		}
		return commentID, nil
	}

	commentID, err = s.github.CreateIssueComment(ctx, github.CreateIssueCommentParams{
		InstallationID: params.installationID,
		Owner:          params.owner,
		Repo:           params.repo,
		PrNumber:       params.prNumber,
		Comment:        params.comment,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to create or update issue comment: %w", err)
	}
	if err := s.setCommentID(ctx, app, commentID); err != nil {
		return 0, fmt.Errorf("failed to save comment ID: %w", err)
	}
	return commentID, nil
}

func (s *Server) updateLastPushSHA(ctx context.Context, app *appsv1alpha1.App, sha string) error {
	if app.Annotations == nil {
		app.Annotations = map[string]string{}
	}
	app.Annotations[config.LastPushAnnotation] = sha
	// 新しいコミットの場合はCheckRunが再度走るように削除する
	app.Annotations[config.CheckRunAnnotation] = ""
	return s.k8s.Update(ctx, app)
}

func branchFromRef(ref string) string {
	const prefix = "refs/heads/"
	if len(ref) > len(prefix) {
		return ref[len(prefix):]
	}
	return ref
}

func compareRepositoryURL(repo1, repo2 string) bool {
	return strings.TrimSuffix(repo1, ".git") == strings.TrimSuffix(repo2, ".git")
}
