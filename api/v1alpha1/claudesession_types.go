package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SpawnMode controls how the remote-control server creates sessions
// +kubebuilder:validation:Enum=same-dir;worktree;session
type SpawnMode string

const (
	SpawnSameDir  SpawnMode = "same-dir"
	SpawnWorktree SpawnMode = "worktree"
	SpawnSession  SpawnMode = "session"
)

// SessionPhase represents the lifecycle phase of a ClaudeSession
// +kubebuilder:validation:Enum=Pending;Running;Failed
type SessionPhase string

const (
	SessionPhasePending SessionPhase = "Pending"
	SessionPhaseRunning SessionPhase = "Running"
	SessionPhaseFailed  SessionPhase = "Failed"
)

// ClaudeSessionSpec defines the desired state of ClaudeSession
type ClaudeSessionSpec struct {
	// SessionName is the display name shown in claude.ai session list
	// +kubebuilder:validation:Required
	SessionName string `json:"sessionName"`

	// Image is the container image with claude CLI pre-installed
	// +kubebuilder:default="ghcr.io/swrm-io/claude-code:latest"
	Image string `json:"image,omitempty"`

	// CredentialsSecret references the Secret containing ~/.claude/.credentials.json
	// obtained via `claude /login`. OAuth is the only supported auth mode for
	// ClaudeSession because Remote Control requires a claude.ai subscription token;
	// API keys cannot be used here.
	// +kubebuilder:validation:Required
	CredentialsSecret SecretKeyRef `json:"credentialsSecret"`

	// WorkDir is the working directory inside the container
	// +kubebuilder:default="/workspace"
	WorkDir string `json:"workDir,omitempty"`

	// Spawn controls session creation strategy for remote-control server mode
	// +kubebuilder:default=same-dir
	Spawn SpawnMode `json:"spawn,omitempty"`

	// Env contains additional environment variables for the session container
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Volumes to mount into the session container
	// +optional
	Volumes []corev1.Volume `json:"volumes,omitempty"`

	// VolumeMounts to attach to the session container
	// +optional
	VolumeMounts []corev1.VolumeMount `json:"volumeMounts,omitempty"`

	// McpServers defines MCP server sidecars to run alongside the Claude container.
	// Each server is accessible to Claude via a Unix socket at /mcp-sockets/<name>.sock.
	// +optional
	McpServers []MCPServer `json:"mcpServers,omitempty"`
}

// ClaudeSessionStatus defines the observed state of ClaudeSession
type ClaudeSessionStatus struct {
	// Phase is the current lifecycle phase of the session
	// +optional
	Phase SessionPhase `json:"phase,omitempty"`

	// PodName is the name of the running session Pod
	// +optional
	PodName string `json:"podName,omitempty"`

	// StartTime is when the session Pod was created
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// Conditions reflect the current state of the ClaudeSession
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cs
// +kubebuilder:printcolumn:name="Session Name",type=string,JSONPath=`.spec.sessionName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.status.podName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClaudeSession is the Schema for the claudesessions API
type ClaudeSession struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClaudeSessionSpec   `json:"spec,omitempty"`
	Status ClaudeSessionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClaudeSessionList contains a list of ClaudeSession
type ClaudeSessionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClaudeSession `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClaudeSession{}, &ClaudeSessionList{})
}
