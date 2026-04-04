package resource

import (
	"fmt"

	appsv1alpha1 "github.com/nomanoma121/snappy/api/v1alpha1"
	"github.com/nomanoma121/snappy/internal/config"
	"github.com/nomanoma121/snappy/internal/github"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func NewAppSecret(app *appsv1alpha1.App, secretName, dockerConfig, installationAccessToken string) *corev1.Secret {
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
			config.InstallationAccessTokenKey: []byte(installationAccessToken),
		},
	}
}

func NewBuildPushImageJob(app *appsv1alpha1.App, jobName, destination, sha, repoSecretName string) *batchv1.Job {
	owner, repo := github.ParseRepoURL(app.Spec.Source.Repo)
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
											Key:                  config.InstallationAccessTokenKey,
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
									Name:      "registry-auth",
									MountPath: "/root/.docker",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "registry-auth",
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

func NewBuildImageJob(app *appsv1alpha1.App, jobName, sha, repoSecretName string) *batchv1.Job {
	owner, repo := github.ParseRepoURL(app.Spec.Source.Repo)
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
											Key:                  config.InstallationAccessTokenKey,
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
								"--output", "type=image,push=false",
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: boolPtr(true),
							},
						},
					},
				},
			},
		},
	}
}

func NewAppDeployment(app *appsv1alpha1.App, image, repoSecretName string) *appsv1.Deployment {
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
