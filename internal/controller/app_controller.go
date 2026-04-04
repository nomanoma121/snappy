/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/nomanoma121/snappy/api/v1alpha1"
	"github.com/nomanoma121/snappy/internal/config"
	forge "github.com/nomanoma121/snappy/internal/forge"
	appsv1 "k8s.io/api/apps/v1"
)

// AppReconciler reconciles a App object
type AppReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	GitHubClient *forge.GitHubClient
	Registry     string // e.g. "ghcr.io/you"
	GhcrPat      string // PAT for pushing to ghcr.io
}

// +kubebuilder:rbac:groups=apps.nomanoma121.github.io,resources=apps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.nomanoma121.github.io,resources=apps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.nomanoma121.github.io,resources=apps/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch

func (r *AppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var app appsv1alpha1.App
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	sha := app.Annotations[config.LastPushAnnotation]
	if sha == "" {
		return ctrl.Result{}, nil
	}

	log.Info("reconciling app", "app", app.Name, "sha", sha)

	checkRunID, err := r.createCheckRun(ctx, &app, sha, forge.CheckStatusInProgress)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create check run: %w", err)
	}

	repoSecretName := fmt.Sprintf("%s-repo-auth", app.Name)
	if err := r.ensureRepoSecret(ctx, &app, repoSecretName); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to ensure registry secret: %w", err)
	}

	result, image, err := r.reconcileBuild(ctx, &app, sha, repoSecretName)
	if err != nil {
		log.Error(err, "build failed", "app", app.Name)
		if err := r.updateCheckRun(ctx, &app, checkRunID, forge.CheckConclusionFailure); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update check run: %w", err)
		}
		return result, err
	}
	if result.RequeueAfter > 0 {
		return result, nil
	}

	if err := r.reconcileDeployment(ctx, &app, image, repoSecretName); err != nil {
		log.Error(err, "deployment failed", "app", app.Name)
		if err := r.updateCheckRun(ctx, &app, checkRunID, forge.CheckConclusionFailure); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update check run: %w", err)
		}
		return ctrl.Result{}, fmt.Errorf("failed to reconcile deployment: %w", err)
	}

	log.Info("app reconciled successfully", "app", app.Name, "image", image)

	if err := r.updateCheckRun(ctx, &app, checkRunID, forge.CheckConclusionSuccess); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update check run: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *AppReconciler) ensureRepoSecret(ctx context.Context, app *appsv1alpha1.App, secretName string) error {
	log := logf.FromContext(ctx)
	log.Info("ensuring registry secret", "secret", secretName)

	installationID, err := r.getInstallationID(ctx)
	if err != nil {
		return err
	}
	token, err := r.GitHubClient.GetInstallationAccessToken(ctx, installationID)
	if err != nil {
		return fmt.Errorf("failed to get installation token: %w", err)
	}

	// TODO: あとでこの辺のリファクタをする
	dockerConfig := fmt.Sprintf(`{"auths":{"ghcr.io":{"username":"x-access-token","password":%q}}}`, r.GhcrPat)

	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: secretName, Namespace: app.Namespace}
	err = r.Get(ctx, key, secret)
	if errors.IsNotFound(err) {
		return r.Create(ctx, r.buildSecret(app, secretName, dockerConfig, token))
	}
	if err != nil {
		return err
	}
	secret.Type = corev1.SecretTypeDockerConfigJson
	secret.Data = map[string][]byte{
		corev1.DockerConfigJsonKey: []byte(dockerConfig),
		"github-token":             []byte(token),
	}
	return r.Update(ctx, secret)
}

func (r *AppReconciler) reconcileBuild(ctx context.Context, app *appsv1alpha1.App, sha, repoSecretName string) (ctrl.Result, string, error) {
	log := logf.FromContext(ctx)
	jobName := fmt.Sprintf("%s-build-%s", app.Name, sha[:8])
	_, repoName := parseRepoURL(app.Spec.Source.Repo)
	destination := fmt.Sprintf("%s/%s:%s", r.Registry, repoName, sha[:8])

	var job batchv1.Job
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: app.Namespace}, &job)
	if errors.IsNotFound(err) {
		log.Info("creating build job", "job", jobName)
		if err := r.Create(ctx, r.buildJob(app, jobName, destination, sha, repoSecretName)); err != nil {
			return ctrl.Result{}, "", err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, "", nil
	}
	if err != nil {
		return ctrl.Result{}, "", err
	}

	if job.Status.Succeeded > 0 {
		log.Info("build job succeeded", "job", jobName)
		return ctrl.Result{}, destination, nil
	}
	if job.Status.Failed > 0 {
		log.Info("build job failed", "job", jobName)
		return ctrl.Result{}, "", fmt.Errorf("build job failed")
	}

	log.Info("build job in progress", "job", jobName)
	return ctrl.Result{RequeueAfter: 10 * time.Second}, "", nil
}

func (r *AppReconciler) reconcileDeployment(ctx context.Context, app *appsv1alpha1.App, image, repoSecretName string) error {
	log := logf.FromContext(ctx)
	var deploy appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, &deploy)
	if errors.IsNotFound(err) {
		log.Info("creating deployment", "app", app.Name, "image", image)
		return r.Create(ctx, buildDeployment(app, image, repoSecretName))
	}
	if err != nil {
		return err
	}

	if *deploy.Spec.Replicas != *app.Spec.Replicas || deploy.Spec.Template.Spec.Containers[0].Image != image {
		log.Info("updating deployment", "app", app.Name, "image", image)
		deploy.Spec.Replicas = app.Spec.Replicas
		deploy.Spec.Template.Spec.Containers[0].Image = image
		return r.Update(ctx, &deploy)
	}

	log.Info("deployment up to date", "app", app.Name)
	return nil
}

func (r *AppReconciler) buildSecret(app *appsv1alpha1.App, secretName, dockerConfig, githubToken string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: app.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(app, appsv1alpha1.GroupVersion.WithKind("App")),
			},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(dockerConfig),
			"github-token":             []byte(githubToken),
		},
	}
}

func (r *AppReconciler) buildJob(app *appsv1alpha1.App, jobName, destination, sha, repoSecretName string) *batchv1.Job {
	owner, repo := parseRepoURL(app.Spec.Source.Repo)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: app.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(app, appsv1alpha1.GroupVersion.WithKind("App")),
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:  "buildkit",
							Image: "moby/buildkit:latest",
							Env: []corev1.EnvVar{
								{
									Name: "GITHUB_TOKEN",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{Name: repoSecretName},
											Key:                  "github-token",
										},
									},
								},
							},
							Command: []string{
								"buildctl-daemonless.sh",
								"build",
								"--frontend=dockerfile.v0",
								"--opt", fmt.Sprintf("context=https://x-access-token:$(GITHUB_TOKEN)@github.com/%s/%s.git#%s", owner, repo, sha),
								"--opt", fmt.Sprintf("filename=%s", app.Spec.Source.DockerfilePath),
								"--output", fmt.Sprintf("type=image,name=%s,push=true", destination),
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: boolPtr(true),
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "repo-auth",
									MountPath: "/root/.docker",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "repo-auth",
							VolumeSource: corev1.VolumeSource{
									Secret: &corev1.SecretVolumeSource{
										SecretName: repoSecretName,
										Items: []corev1.KeyToPath{
											{
												Key:  corev1.DockerConfigJsonKey,
												Path: "config.json",
											},
										},
									},
							},
						},
					},
				},
			},
		},
	}
}

func buildDeployment(app *appsv1alpha1.App, image, repoSecretName string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(app, appsv1alpha1.GroupVersion.WithKind("App")),
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: app.Spec.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": app.Name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": app.Name},
				},
				Spec: corev1.PodSpec{
					ImagePullSecrets: []corev1.LocalObjectReference{
						{Name: repoSecretName},
					},
					Containers: []corev1.Container{
						{
							Name:    "app",
							Image:   image,
							Env:     app.Spec.Env,
							EnvFrom: app.Spec.EnvFrom,
						},
					},
				},
			},
		},
	}
}

func boolPtr(b bool) *bool { return &b }

func (r *AppReconciler) getInstallationID(ctx context.Context) (int64, error) {
	var secret corev1.Secret
	// TODO: 名前分かりにくいのでtokenじゃなくてinstallation_idにしたい
	if err := r.Get(ctx, types.NamespacedName{Name: config.TokenSecretName, Namespace: config.TokenSecretNS}, &secret); err != nil {
		return 0, fmt.Errorf("failed to get github token secret: %w", err)
	}
	var id int64
	if _, err := fmt.Sscan(string(secret.Data["installation_id"]), &id); err != nil {
		return 0, fmt.Errorf("invalid installation_id: %w", err)
	}
	return id, nil
}

func (r *AppReconciler) createCheckRun(ctx context.Context, app *appsv1alpha1.App, sha string, status forge.CheckStatus) (int64, error) {
	log := logf.FromContext(ctx)
	installationID, err := r.getInstallationID(ctx)
	if err != nil {
		return 0, err
	}
	owner, repo := parseRepoURL(app.Spec.Source.Repo)

	// ReconcileのたびにCheckRunが走ってしまうため、過去に作成していた場合IDをそのまま返す
	checkRunId := app.Annotations[config.CheckRunAnnotation]
	if checkRunId != "" {
		log.Info("reusing existing check run", "checkRunId", checkRunId)
		return strconv.ParseInt(checkRunId, 10, 64)
	}

	id, err := r.GitHubClient.CreateCheckRun(ctx, installationID, owner, repo, sha, "deploy", status)
	if err != nil {
		return 0, fmt.Errorf("failed to create check run: %w", err)
	}

	app.Annotations[config.CheckRunAnnotation] = strconv.FormatInt(id, 10)
	return id, r.Update(ctx, app)
}

func (r *AppReconciler) updateCheckRun(ctx context.Context, app *appsv1alpha1.App, checkRunID int64, conclusion forge.CheckConclusion) error {
	installationID, err := r.getInstallationID(ctx)
	if err != nil {
		return err
	}
	owner, repo := parseRepoURL(app.Spec.Source.Repo)
	return r.GitHubClient.UpdateCheckRun(ctx, installationID, owner, repo, checkRunID, conclusion)
}

// parseRepoURL extracts owner and repo from a GitHub URL.
// e.g. "https://github.com/owner/repo" → ("owner", "repo")
func parseRepoURL(repoURL string) (owner, repo string) {
	repoURL = strings.TrimSuffix(repoURL, ".git")
	parts := strings.Split(repoURL, "/")
	if len(parts) < 2 {
		return "", ""
	}
	return parts[len(parts)-2], parts[len(parts)-1]
}

// SetupWithManager sets up the controller with the Manager.
func (r *AppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.App{}).
		Owns(&batchv1.Job{}).
		Named("app").
		Complete(r)
}
