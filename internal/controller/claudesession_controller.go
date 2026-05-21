package controller

import (
	"context"
	"fmt"

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
	claudeSessionFinalizer = "agentic.swrm.io/claudesession-finalizer"
)

// ClaudeSessionReconciler reconciles a ClaudeSession object
type ClaudeSessionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agentic.swrm.io,resources=claudesessions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentic.swrm.io,resources=claudesessions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentic.swrm.io,resources=claudesessions/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;patch

func (r *ClaudeSessionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	session := &agenticiov1alpha1.ClaudeSession{}
	if err := r.Get(ctx, req.NamespacedName, session); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !session.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.handleDeletion(ctx, session)
	}

	// Ensure finalizer
	if !controllerutil.ContainsFinalizer(session, claudeSessionFinalizer) {
		controllerutil.AddFinalizer(session, claudeSessionFinalizer)
		if err := r.Update(ctx, session); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Reconcile the owned Pod
	pod, err := r.reconcilePod(ctx, session)
	if err != nil {
		logger.Error(err, "failed to reconcile session Pod")
		return ctrl.Result{}, err
	}

	// Update status based on pod phase
	patch := client.MergeFrom(session.DeepCopy())
	session.Status.PodName = pod.Name
	session.Status.Phase = podPhaseToSessionPhase(pod.Status.Phase)
	if pod.Status.StartTime != nil && session.Status.StartTime == nil {
		session.Status.StartTime = pod.Status.StartTime
	}
	if err := r.Status().Patch(ctx, session, patch); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *ClaudeSessionReconciler) reconcilePod(ctx context.Context, session *agenticiov1alpha1.ClaudeSession) (*corev1.Pod, error) {
	desired := r.buildPod(session)
	if err := controllerutil.SetControllerReference(session, desired, r.Scheme); err != nil {
		return nil, err
	}

	existing := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return nil, fmt.Errorf("creating session Pod: %w", err)
		}
		return desired, nil
	}
	if err != nil {
		return nil, err
	}

	// Pods are largely immutable — only recreate if the image changed
	if existing.Spec.Containers[0].Image != session.Spec.Image {
		if err := r.Delete(ctx, existing); err != nil {
			return nil, fmt.Errorf("deleting stale session Pod: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return nil, fmt.Errorf("recreating session Pod: %w", err)
		}
		return desired, nil
	}

	return existing, nil
}

func (r *ClaudeSessionReconciler) buildPod(session *agenticiov1alpha1.ClaudeSession) *corev1.Pod {
	credentialsVolumeName := "claude-credentials"
	claudeHomeVolume := "claude-home"

	volumes := append([]corev1.Volume{
		{
			Name: credentialsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: session.Spec.CredentialsSecret.Name,
					Items: []corev1.KeyToPath{
						{
							Key:  session.Spec.CredentialsSecret.Key,
							Path: "credentials.json",
						},
					},
				},
			},
		},
		{
			// emptyDir shared between init container and main container.
			// The init container writes ~/.claude.json with minimal account
			// metadata (organizationUuid) that claude requires for Remote
			// Control eligibility checks.
			Name:         claudeHomeVolume,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}, session.Spec.Volumes...)

	volumeMounts := append([]corev1.VolumeMount{
		{
			Name:      credentialsVolumeName,
			MountPath: claudeConfigDir + "/.credentials.json",
			SubPath:   "credentials.json",
			ReadOnly:  true,
		},
		{
			Name:      claudeHomeVolume,
			MountPath: "/home/node/.claude.json",
			SubPath:   ".claude.json",
		},
	}, session.Spec.VolumeMounts...)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      session.Name,
			Namespace: session.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":  "agentic-operator",
				"agentic.swrm.io/claudesession": session.Name,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			Volumes:       volumes,
			InitContainers: []corev1.Container{
				{
					Name:    "claude-config-init",
					Image:   "busybox:1",
					Command: []string{"sh", "-c"},
					Args: []string{
						`printf '{"hasCompletedOnboarding":true,"remoteControlAtStartup":true,"oauthAccount":{"organizationUuid":"%s"},"projects":{"%s":{"hasTrustDialogAccepted":true,"remoteDialogSeen":true,"allowedTools":[],"mcpServers":{}}}}' "$ORG_ID" "$WORK_DIR" > /claude-home/.claude.json`,
					},
					Env: []corev1.EnvVar{
						{
							Name: "ORG_ID",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: session.Spec.CredentialsSecret.Name,
									},
									Key: "organizationId",
								},
							},
						},
						{
							Name:  "WORK_DIR",
							Value: session.Spec.WorkDir,
						},
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      claudeHomeVolume,
							MountPath: "/claude-home",
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:         "claude",
					Image:        session.Spec.Image,
					WorkingDir:   session.Spec.WorkDir,
					Env:          session.Spec.Env,
					VolumeMounts: volumeMounts,
					Command: []string{
						"claude",
						"remote-control",
						"--name", session.Spec.SessionName,
						"--spawn", string(session.Spec.Spawn),
					},
				},
			},
		},
	}
}

func (r *ClaudeSessionReconciler) handleDeletion(ctx context.Context, session *agenticiov1alpha1.ClaudeSession) error {
	if controllerutil.ContainsFinalizer(session, claudeSessionFinalizer) {
		controllerutil.RemoveFinalizer(session, claudeSessionFinalizer)
		return r.Update(ctx, session)
	}
	return nil
}

func podPhaseToSessionPhase(phase corev1.PodPhase) agenticiov1alpha1.SessionPhase {
	switch phase {
	case corev1.PodRunning:
		return agenticiov1alpha1.SessionPhaseRunning
	case corev1.PodFailed:
		return agenticiov1alpha1.SessionPhaseFailed
	default:
		return agenticiov1alpha1.SessionPhasePending
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClaudeSessionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agenticiov1alpha1.ClaudeSession{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}
