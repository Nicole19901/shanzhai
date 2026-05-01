//go:build windows

package webui

import (
	"fmt"
	"os/exec"
)

// runServiceControl 在 Windows 上使用 sc.exe 控制服务。
func (s *Server) runServiceControl(action string) (string, error) {
	if s.serviceName == "" {
		return "", fmt.Errorf("未配置服务名称，无法执行服务控制")
	}
	var scAction string
	switch action {
	case "start":
		scAction = "start"
	case "stop":
		scAction = "stop"
	case "restart":
		cmdDesc := fmt.Sprintf("sc stop %s && sc start %s", s.serviceName, s.serviceName)
		_ = exec.Command("sc", "stop", s.serviceName).Run()
		if err := exec.Command("sc", "start", s.serviceName).Run(); err != nil {
			return cmdDesc, fmt.Errorf("重启失败（错误：%w）\n请确认：①以管理员权限运行；②服务 '%s' 已通过 sc create 安装为 Windows 服务", err, s.serviceName)
		}
		return cmdDesc, nil
	default:
		return "", fmt.Errorf("不支持的操作：%s", action)
	}
	cmdDesc := fmt.Sprintf("sc %s %s", scAction, s.serviceName)
	if err := exec.Command("sc", scAction, s.serviceName).Run(); err != nil {
		return cmdDesc, fmt.Errorf("%s 失败（错误：%w）\n请确认：①以管理员权限运行；②服务 '%s' 已通过 sc create 安装为 Windows 服务；③若直接运行 .exe 无需使用此按钮", err, scAction, s.serviceName)
	}
	return cmdDesc, nil
}
