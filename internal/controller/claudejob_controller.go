package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agenticiov1alpha1 "github.com/swrm-io/agentic-operator/api/v1alpha1"
)

const (
	claudeConfigDir    = "/home/node/.claude"
	claudeJobFinalizer = "agentic.swrm.io/claudejob-finalizer"
)

// ClaudeJobReconciler reconciles a ClaudeJob object
type ClaudeJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agentic.swrm.io,resources=claudejobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentic.swrm.io,resources=claudejobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentic.swrm.io,resources=claudejobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;patch

func (r *ClaudeJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	job := &agenticiov1alpha1.ClaudeJob{}
	if err := r.Get(ctx, req.NamespacedName, job); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !job.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.handleDeletion(ctx, job)
	}

	// Ensure finalizer
	if !controllerutil.ContainsFinalizer(job, claudeJobFinalizer) {
		controllerutil.AddFinalizer(job, claudeJobFinalizer)
		if err := r.Update(ctx, job); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Validate auth config — exactly one must be set
	if err := validateAuth(job); err != nil {
		logger.Error(err, "invalid auth configuration")
		return ctrl.Result{}, err
	}

	// Reconcile the owned CronJob
	cronJob, err := r.reconcileCronJob(ctx, job)
	if err != nil {
		logger.Error(err, "failed to reconcile CronJob")
		return ctrl.Result{}, err
	}

	// Update status
	patch := client.MergeFrom(job.DeepCopy())
	job.Status.CronJobName = cronJob.Name
	job.Status.LastScheduleTime = cronJob.Status.LastScheduleTime
	job.Status.LastSuccessfulTime = cronJob.Status.LastSuccessfulTime
	if err := r.Status().Patch(ctx, job, patch); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *ClaudeJobReconciler) reconcileCronJob(ctx context.Context, job *agenticiov1alpha1.ClaudeJob) (*batchv1.CronJob, error) {
	desired := r.buildCronJob(job)
	if err := controllerutil.SetControllerReference(job, desired, r.Scheme); err != nil {
		return nil, err
	}

	existing := &batchv1.CronJob{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return nil, fmt.Errorf("creating CronJob: %w", err)
		}
		return desired, nil
	}
	if err != nil {
		return nil, err
	}

	// Update mutable fields
	existing.Spec.Schedule = desired.Spec.Schedule
	existing.Spec.Suspend = desired.Spec.Suspend
	existing.Spec.JobTemplate = desired.Spec.JobTemplate
	if err := r.Update(ctx, existing); err != nil {
		return nil, fmt.Errorf("updating CronJob: %w", err)
	}
	return existing, nil
}

func (r *ClaudeJobReconciler) buildCronJob(job *agenticiov1alpha1.ClaudeJob) *batchv1.CronJob {
	suspend := job.Spec.Suspend

	volumes, volumeMounts, extraEnv := r.buildAuthMounts(job)
	volumes = append(volumes, job.Spec.Volumes...)
	volumeMounts = append(volumeMounts, job.Spec.VolumeMounts...)
	env := append(extraEnv, job.Spec.Env...)

	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name,
			Namespace: job.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "agentic-operator",
				"agentic.swrm.io/claudejob":   job.Name,
			},
		},
		Spec: batchv1.CronJobSpec{
			Schedule: job.Spec.Schedule,
			Suspend:  &suspend,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyOnFailure,
							Volumes:       volumes,
							InitContainers: r.buildConfigInitContainer(job),
							Containers: []corev1.Container{
								{
									Name:         "claude",
									Image:        job.Spec.Image,
									WorkingDir:   job.Spec.WorkDir,
									Env:          env,
									VolumeMounts: volumeMounts,
									Command: []string{
										"claude",
										"--print",
										"--dangerously-skip-permissions",
										job.Spec.Prompt,
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

// buildConfigInitContainer returns an init container that writes a minimal
// ~/.claude.json containing the organizationUuid. Only used in OAuth mode.
func (r *ClaudeJobReconciler) buildConfigInitContainer(job *agenticiov1alpha1.ClaudeJob) []corev1.Container {
	if job.Spec.Auth.CredentialsSecret == nil {
		return nil
	}
	return []corev1.Container{
		{
			Name:    "claude-config-init",
			Image:   "busybox:1",
			Command: []string{"sh", "-c"},
			Args: []string{
				`printf '{"hasCompletedOnboarding":true,"remoteDialogSeen":true,"remoteControlAtStartup":true,"oauthAccount":{"organizationUuid":"%s"},"projects":{"%s":{"hasTrustDialogAccepted":true,"allowedTools":[],"mcpServers":{}}}}' "$ORG_ID" "$WORK_DIR" > /claude-home/.claude.json`,
			},
			Env: []corev1.EnvVar{
				{
					Name: "ORG_ID",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: job.Spec.Auth.CredentialsSecret.Name,
							},
							Key: "organizationId",
						},
					},
				},
				{
					Name:  "WORK_DIR",
					Value: job.Spec.WorkDir,
				},
			},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "claude-home",
					MountPath: "/claude-home",
				},
			},
		},
	}
}

// buildAuthMounts returns the volumes, volumeMounts, and env vars needed for
// whichever auth mode the job is configured with.
func (r *ClaudeJobReconciler) buildAuthMounts(job *agenticiov1alpha1.ClaudeJob) ([]corev1.Volume, []corev1.VolumeMount, []corev1.EnvVar) {
	if job.Spec.Auth.CredentialsSecret != nil {
		// OAuth mode: mount credentials file and a generated minimal .claude.json.
		// claude checks oauthAccount.organizationUuid in ~/.claude.json for feature
		// eligibility. An init container writes a minimal file from the organizationId
		// secret key so we avoid storing the full ~/.claude.json in the secret.
		vols := []corev1.Volume{
			{
				Name: "claude-credentials",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: job.Spec.Auth.CredentialsSecret.Name,
						Items: []corev1.KeyToPath{
							{
								Key:  job.Spec.Auth.CredentialsSecret.Key,
								Path: "credentials.json",
							},
						},
					},
				},
			},
			{
				Name:         "claude-home",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
		}
		mounts := []corev1.VolumeMount{
			{
				Name:      "claude-credentials",
				MountPath: claudeConfigDir + "/.credentials.json",
				SubPath:   "credentials.json",
				ReadOnly:  true,
			},
			{
				Name:      "claude-home",
				MountPath: "/home/node/.claude.json",
				SubPath:   ".claude.json",
			},
		}
		return vols, mounts, nil
	}

	// API key mode: inject ANTHROPIC_API_KEY from secret
	env := corev1.EnvVar{
		Name: "ANTHROPIC_API_KEY",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: job.Spec.Auth.APIKeySecret.Name,
				},
				Key: job.Spec.Auth.APIKeySecret.Key,
			},
		},
	}
	return nil, nil, []corev1.EnvVar{env}
}

func validateAuth(job *agenticiov1alpha1.ClaudeJob) error {
	hasOAuth := job.Spec.Auth.CredentialsSecret != nil
	hasAPIKey := job.Spec.Auth.APIKeySecret != nil
	if hasOAuth == hasAPIKey {
		return fmt.Errorf("exactly one of spec.auth.credentialsSecret or spec.auth.apiKeySecret must be set")
	}
	return nil
}

func (r *ClaudeJobReconciler) handleDeletion(ctx context.Context, job *agenticiov1alpha1.ClaudeJob) error {
	if controllerutil.ContainsFinalizer(job, claudeJobFinalizer) {
		controllerutil.RemoveFinalizer(job, claudeJobFinalizer)
		return r.Update(ctx, job)
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClaudeJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agenticiov1alpha1.ClaudeJob{}).
		Owns(&batchv1.CronJob{}).
		Complete(r)
}
