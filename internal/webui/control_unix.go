//go:build !windows

package webui

import (
	"fmt"
	"os/exec"
)

// runServiceControl 在 Linux/macOS 上使用 systemctl 控制服务。
func (s *Server) runServiceControl(action string) (string, error) {
	if s.serviceName == "" {
		return "", fmt.Errorf("service name not configured")
	}
	switch action {
	case "start", "stop", "restart":
	default:
		return "", fmt.Errorf("unsupported action: %s", action)
	}
	cmdDesc := fmt.Sprintf("systemctl %s %s", action, s.serviceName)
	if err := exec.Command("systemctl", action, s.serviceName).Run(); err != nil {
		return cmdDesc, fmt.Errorf("systemctl %s failed: %w", action, err)
	}
	return cmdDesc, nil
}
