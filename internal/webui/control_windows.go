//go:build windows

package webui

import (
	"fmt"
	"os/exec"
)

// runServiceControl 在 Windows 上使用 sc.exe 控制服务。
// 返回实际执行的命令描述和错误。
func (s *Server) runServiceControl(action string) (string, error) {
	if s.serviceName == "" {
		return "", fmt.Errorf("service name not configured")
	}
	var scAction string
	switch action {
	case "start":
		scAction = "start"
	case "stop":
		scAction = "stop"
	case "restart":
		// Windows sc.exe 没有 restart，先 stop 再 start
		cmdDesc := fmt.Sprintf("sc stop %s && sc start %s", s.serviceName, s.serviceName)
		if err := exec.Command("sc", "stop", s.serviceName).Run(); err != nil {
			// stop 失败时仍然尝试 start（服务可能已经停止）
			_ = err
		}
		if err := exec.Command("sc", "start", s.serviceName).Run(); err != nil {
			return cmdDesc, fmt.Errorf("sc start failed: %w", err)
		}
		return cmdDesc, nil
	default:
		return "", fmt.Errorf("unsupported action: %s", action)
	}
	cmdDesc := fmt.Sprintf("sc %s %s", scAction, s.serviceName)
	if err := exec.Command("sc", scAction, s.serviceName).Run(); err != nil {
		return cmdDesc, fmt.Errorf("sc %s failed: %w", scAction, err)
	}
	return cmdDesc, nil
}
