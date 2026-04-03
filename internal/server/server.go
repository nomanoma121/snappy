package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	appsv1alpha1 "github.com/nomanoma121/snappy/api/v1alpha1"
	"github.com/nomanoma121/snappy/internal/config"
	"github.com/nomanoma121/snappy/internal/forge"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	githubAppName   = "snappy-release"
)

type Server struct {
	router chi.Router
	addr   string
	github *forge.GitHubClient
	k8s    client.Client
}

func NewServer(addr string, gh *forge.GitHubClient, k8s client.Client) *Server {
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
	http.Redirect(w, r, fmt.Sprintf("https://github.com/apps/%s/installations/new", githubAppName), http.StatusFound)
}

func (s *Server) Callback(w http.ResponseWriter, r *http.Request) {
	installationIDStr := r.URL.Query().Get("installation_id")
	if installationIDStr == "" {
		http.Error(w, "installation_id is required", http.StatusBadRequest)
		return
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.TokenSecretName,
			Namespace: config.TokenSecretNS,
		},
		Data: map[string][]byte{
			"installation_id": []byte(installationIDStr),
		},
	}
	if err := s.k8s.Create(r.Context(), secret); err != nil {
		http.Error(w, "failed to save installation_id", http.StatusInternalServerError)
		return
	}

	w.Write([]byte("GitHub App installed successfully"))
}

type pushPayload struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Repository struct {
		CloneURL string `json:"clone_url"`
	} `json:"repository"`
}

func (s *Server) Webhook(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-GitHub-Event") != "push" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var payload pushPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	branch := branchFromRef(payload.Ref)
	appList := &appsv1alpha1.AppList{}
	if err := s.k8s.List(r.Context(), appList); err != nil {
		http.Error(w, "failed to list apps", http.StatusInternalServerError)
		return
	}

	for _, app := range appList.Items {
		if s.compareRepositoryURL(app.Spec.Source.Repo, payload.Repository.CloneURL) && app.Spec.Source.Branch == branch {
			if err := s.updateLastPushSHA(r.Context(), &app, payload.After); err != nil {
				http.Error(w, "failed to update app", http.StatusInternalServerError)
				return
			}
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateLastPushSHA(ctx context.Context, app *appsv1alpha1.App, sha string) error {
	if app.Annotations == nil {
		app.Annotations = map[string]string{}
	}
	app.Annotations["snappy/last-push-sha"] = sha
	return s.k8s.Update(ctx, app)
}

func branchFromRef(ref string) string {
	const prefix = "refs/heads/"
	if len(ref) > len(prefix) {
		return ref[len(prefix):]
	}
	return ref
}

func (s *Server) installationID(ctx context.Context) (int64, error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Name: config.TokenSecretName, Namespace: config.TokenSecretNS}
	if err := s.k8s.Get(ctx, key, secret); err != nil {
		return 0, err
	}
	return strconv.ParseInt(string(secret.Data["installation_id"]), 10, 64)
}

func (s *Server) compareRepositoryURL(repo1, repo2 string) bool {
	return strings.TrimSuffix(repo1, ".git") == strings.TrimSuffix(repo2, ".git")
}
