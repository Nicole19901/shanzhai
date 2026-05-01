//go:build !windows

package webui

import (
	"errors"
	"fmt"
	"os/exec"
)

// runServiceControl 在 Linux/macOS 上使用 systemctl 控制服务。
// systemctl 退出码含义：0=成功 1=通用错误 3=服务未运行 4=无权限 5=服务未找到
func (s *Server) runServiceControl(action string) (string, error) {
	if s.serviceName == "" {
		return "", fmt.Errorf("未配置服务名称")
	}
	switch action {
	case "start", "stop", "restart":
	default:
		return "", fmt.Errorf("不支持的操作：%s", action)
	}
	cmdDesc := fmt.Sprintf("systemctl %s %s", action, s.serviceName)
	err := exec.Command("systemctl", action, s.serviceName).Run()
	if err == nil {
		return cmdDesc, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		switch exitErr.ExitCode() {
		case 5:
			return cmdDesc, fmt.Errorf(
				"服务 '%s' 未注册（systemctl 退出码 5）。\n"+
					"若您是直接运行可执行文件，无需使用此按钮。\n"+
					"如需注册为系统服务，请创建 /etc/systemd/system/%s.service 后执行 systemctl daemon-reload",
				s.serviceName, s.serviceName)
		case 4:
			return cmdDesc, fmt.Errorf("权限不足（退出码 4），请以 root 或 sudo 运行")
		case 3:
			if action == "stop" {
				// 服务本来就没在运行，stop 视为成功
				return cmdDesc, nil
			}
			return cmdDesc, fmt.Errorf("服务 '%s' 当前未运行（退出码 3）", s.serviceName)
		default:
			return cmdDesc, fmt.Errorf("systemctl %s 失败（退出码 %d）：%w", action, exitErr.ExitCode(), err)
		}
	}
	return cmdDesc, fmt.Errorf("执行 systemctl 失败：%w", err)
}
