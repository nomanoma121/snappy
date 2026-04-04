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
			Name:      config.InstallationIDSecretName,
			Namespace: config.InstallationIDSecretNS,
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
			if err := s.updateLastPushSHA(ctx, &app, *payload.After); err != nil {
				log.Printf("failed to update app: %v", err)
				http.Error(w, "failed to update app", http.StatusInternalServerError)
				return
			}
		}
	}

	w.WriteHeader(http.StatusNoContent)
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
	app, err := s.lookupApp(ctx, *payload.Repo.CloneURL, branchFromRef(*payload.PullRequest.Base.Ref))
	if err != nil {
		log.Printf("failed to lookup app: %v", err)
		http.Error(w, "app not found", http.StatusNotFound)
		return
	}
	installationID, err := s.getInstallationID(ctx)
	if err != nil {
		log.Printf("failed to get installation ID: %v", err)
		http.Error(w, "failed to get installation ID", http.StatusInternalServerError)
		return
	}

	checkRunID, err := s.github.CreateCheckRun(ctx, installationID, *payload.Repo.Owner.Login, *payload.Repo.Name, *payload.PullRequest.Head.SHA, "deploy", github.CheckStatusInProgress)
	if err != nil {
		log.Printf("failed to create check run: %v", err)
		http.Error(w, "failed to create check run", http.StatusInternalServerError)
		return
	}

	sha := *payload.PullRequest.Head.SHA
	job := resource.NewBuildImageJob(app, config.BuildImageJobName(app.Name, sha), sha, config.RepoSecretName(app.Name))
	if err := s.k8s.Create(ctx, job); err != nil {
		log.Printf("failed to create job: %v", err)
		http.Error(w, "failed to create job", http.StatusInternalServerError)
		return
	}

	go s.watchJob(job.Name, job.Namespace, func(succeeded bool) {
		var conclusion github.CheckConclusion
		if succeeded {
			conclusion = github.CheckConclusionSuccess
		} else {
			conclusion = github.CheckConclusionFailure
		}
		if err := s.github.UpdateCheckRun(context.Background(), installationID, *payload.Repo.Owner.Login, *payload.Repo.Name, checkRunID, conclusion); err != nil {
			log.Printf("failed to update check run: %v", err)
		}
	})

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) watchJob(jobName, namespace string, fn func(succeeded bool)) {
	for {
		var job batchv1.Job
		if err := s.k8s.Get(context.Background(), client.ObjectKey{Name: jobName, Namespace: namespace}, &job); err != nil {
			log.Printf("failed to get job: %v", err)
			return
		}
		if job.Status.Succeeded > 0 {
			fn(true)
			return
		}
		if job.Status.Failed > 0 {
			fn(false)
			return
		}
		time.Sleep(1 * time.Second)
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
	if err := s.k8s.Get(ctx, client.ObjectKey{Name: config.InstallationIDSecretName, Namespace: config.InstallationIDSecretNS}, &secret); err != nil {
		log.Printf("failed to get installation ID secret: %v", err)
		return 0, fmt.Errorf("failed to get installation ID secret: %w", err)
	}
	return strconv.ParseInt(string(secret.Data[config.InstallationIDKey]), 10, 64)
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
