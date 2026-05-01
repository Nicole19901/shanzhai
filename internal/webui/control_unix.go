//go:build !windows

package webui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"
	"bytes"
)

const serviceTemplate = `[Unit]
Description=ETH Perp Trading System
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory={{.WorkDir}}
ExecStart={{.ExecPath}}
Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`

func autoRegisterService(serviceName string) error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取可执行文件路径失败：%w", err)
	}
	execPath, _ = filepath.EvalSymlinks(execPath)
	workDir := filepath.Dir(execPath)

	tmpl, err := template.New("svc").Parse(serviceTemplate)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct{ WorkDir, ExecPath string }{workDir, execPath}); err != nil {
		return err
	}

	unitPath := fmt.Sprintf("/etc/systemd/system/%s.service", serviceName)
	if err := os.WriteFile(unitPath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("写入服务文件失败（需要 root 权限）：%w", err)
	}
	if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
		return fmt.Errorf("daemon-reload 失败：%w", err)
	}
	if err := exec.Command("systemctl", "enable", serviceName).Run(); err != nil {
		return fmt.Errorf("enable 失败：%w", err)
	}
	return nil
}

// runServiceControl 在 Linux/macOS 上使用 systemctl 控制服务。
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
	if !errors.As(err, &exitErr) {
		return cmdDesc, fmt.Errorf("执行 systemctl 失败：%w", err)
	}

	switch exitErr.ExitCode() {
	case 5:
		// 服务未注册，尝试自动创建并重试
		if regErr := autoRegisterService(s.serviceName); regErr != nil {
			return cmdDesc, fmt.Errorf("自动注册服务失败：%w\n请确认以 root 权限运行", regErr)
		}
		// 重试原始操作
		if retryErr := exec.Command("systemctl", action, s.serviceName).Run(); retryErr != nil {
			return cmdDesc, fmt.Errorf("服务已自动注册，但 %s 仍然失败：%w", action, retryErr)
		}
		return cmdDesc + "（服务已自动注册）", nil
	case 4:
		return cmdDesc, fmt.Errorf("权限不足，请以 root 或 sudo 运行本程序")
	case 3:
		if action == "stop" {
			return cmdDesc, nil // 已停止视为成功
		}
		return cmdDesc, fmt.Errorf("服务当前未运行，请先检查日志：journalctl -u %s -n 50", s.serviceName)
	default:
		return cmdDesc, fmt.Errorf("systemctl %s 失败（退出码 %d）：%w", action, exitErr.ExitCode(), err)
	}
}
