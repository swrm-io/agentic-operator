package controller

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	agenticiov1alpha1 "github.com/swrm-io/agentic-operator/api/v1alpha1"
)

const mcpSocketsDir = "/mcp-sockets"

// buildMCPSidecars returns the shared socket volume, the volume mount for the
// claude container, and a sidecar container for each MCPServer. Returns nils
// if no MCP servers are defined.
func buildMCPSidecars(servers []agenticiov1alpha1.MCPServer) (*corev1.Volume, corev1.VolumeMount, []corev1.Container) {
	if len(servers) == 0 {
		return nil, corev1.VolumeMount{}, nil
	}

	vol := &corev1.Volume{
		Name:         "mcp-sockets",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
	mount := corev1.VolumeMount{
		Name:      "mcp-sockets",
		MountPath: mcpSocketsDir,
	}

	sidecars := make([]corev1.Container, len(servers))
	for i, s := range servers {
		c := corev1.Container{
			Name:  fmt.Sprintf("mcp-%s", s.Name),
			Image: s.Image,
			Env:   s.Env,
			Args:  s.Args,
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "mcp-sockets",
					MountPath: mcpSocketsDir,
				},
			},
		}
		sidecars[i] = c
	}

	return vol, mount, sidecars
}

// buildClaudeJSONScript returns the sh -c script for the init container.
// It writes .claude.json with mcpServers entries for each declared MCP server.
func buildClaudeJSONScript(workDir string, servers []agenticiov1alpha1.MCPServer) string {
	mcpEntries := make([]string, len(servers))
	for i, s := range servers {
		mcpEntries[i] = fmt.Sprintf(`"%s":{"type":"unix","path":"%s/%s.sock"}`, s.Name, mcpSocketsDir, s.Name)
	}
	mcpJSON := "{" + strings.Join(mcpEntries, ",") + "}"

	return fmt.Sprintf(
		`printf '{"hasCompletedOnboarding":true,"remoteDialogSeen":true,"remoteControlAtStartup":true,"oauthAccount":{"organizationUuid":"%%s"},"projects":{"%%s":{"hasTrustDialogAccepted":true,"allowedTools":[],"mcpServers":%s}}}' "$ORG_ID" "$WORK_DIR" > /claude-home/.claude.json`,
		mcpJSON,
	)
}
