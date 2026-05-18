package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClaudeAuthMode describes which authentication method a ClaudeJob uses.
// +kubebuilder:validation:Enum=OAuth;APIKey
type ClaudeAuthMode string

const (
	// ClaudeAuthModeOAuth uses a claude.ai OAuth login stored in a credentials secret.
	// Required for ClaudeSession (Remote Control). Supports token auto-refresh.
	ClaudeAuthModeOAuth ClaudeAuthMode = "OAuth"

	// ClaudeAuthModeAPIKey injects ANTHROPIC_API_KEY from a secret. Simpler but
	// does not support Remote Control sessions.
	ClaudeAuthModeAPIKey ClaudeAuthMode = "APIKey"
)

// ClaudeJobSpec defines the desired state of ClaudeJob
type ClaudeJobSpec struct {
	// Schedule is a cron expression for when to run the job (e.g. "0 9 * * *")
	// +kubebuilder:validation:Required
	Schedule string `json:"schedule"`

	// Prompt is the instruction to pass to Claude Code when the job runs
	// +kubebuilder:validation:Required
	Prompt string `json:"prompt"`

	// Image is the container image with claude CLI pre-installed
	// +kubebuilder:default="ghcr.io/swrm-io/claude-code:latest"
	Image string `json:"image,omitempty"`

	// Auth configures how the job authenticates with Anthropic.
	// Exactly one of auth.credentialsSecret or auth.apiKeySecret must be set.
	// +kubebuilder:validation:Required
	Auth ClaudeJobAuth `json:"auth"`

	// WorkDir is the working directory inside the container
	// +kubebuilder:default="/workspace"
	WorkDir string `json:"workDir,omitempty"`

	// Env contains additional environment variables to set in the job container
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Volumes to mount into the job container
	// +optional
	Volumes []corev1.Volume `json:"volumes,omitempty"`

	// VolumeMounts to attach to the job container
	// +optional
	VolumeMounts []corev1.VolumeMount `json:"volumeMounts,omitempty"`

	// Suspend stops the CronJob from firing when true
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// ClaudeJobAuth holds the authentication configuration for a ClaudeJob.
// Exactly one field must be set.
type ClaudeJobAuth struct {
	// CredentialsSecret references a Secret containing ~/.claude/.credentials.json
	// obtained via `claude /login`. Supports OAuth token auto-refresh.
	// Mutually exclusive with apiKeySecret.
	// +optional
	CredentialsSecret *SecretKeyRef `json:"credentialsSecret,omitempty"`

	// APIKeySecret references a Secret containing an Anthropic API key.
	// The key value is injected as ANTHROPIC_API_KEY. Does not support
	// Remote Control sessions or token auto-refresh.
	// Mutually exclusive with credentialsSecret.
	// +optional
	APIKeySecret *SecretKeyRef `json:"apiKeySecret,omitempty"`
}

// SecretKeyRef identifies a key within a Kubernetes Secret
type SecretKeyRef struct {
	// Name of the Secret
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Key within the Secret
	// +kubebuilder:validation:Required
	Key string `json:"key"`
}

// ClaudeJobStatus defines the observed state of ClaudeJob
type ClaudeJobStatus struct {
	// CronJobName is the name of the managed CronJob
	// +optional
	CronJobName string `json:"cronJobName,omitempty"`

	// LastScheduleTime is the last time the CronJob was scheduled
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// LastSuccessfulTime is the last time the job completed successfully
	// +optional
	LastSuccessfulTime *metav1.Time `json:"lastSuccessfulTime,omitempty"`

	// Conditions reflect the current state of the ClaudeJob
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cj
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Last Schedule",type=date,JSONPath=`.status.lastScheduleTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClaudeJob is the Schema for the claudejobs API
type ClaudeJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClaudeJobSpec   `json:"spec,omitempty"`
	Status ClaudeJobStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClaudeJobList contains a list of ClaudeJob
type ClaudeJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClaudeJob `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClaudeJob{}, &ClaudeJobList{})
}
