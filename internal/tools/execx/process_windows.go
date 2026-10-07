//go:build windows

package execx

import (
	"fmt"
	"os/exec"
	"strconv"
)

// configureProcessGroup 在 Windows 上为 no-op：Windows 无 POSIX 进程组语义
// （不支持 Setpgid），进程树终止交给 taskkill /T 按进程树遍历。
func configureProcessGroup(cmd *exec.Cmd) {}

// terminateProcessTreePlatform 优雅终止：taskkill /PID <pid> /T（不带 /F）向进程树发送 WM_CLOSE，
// GUI 进程可优雅退出；控制台进程通常忽略该消息，由 Stop 的宽限期升级强杀兜底。
func terminateProcessTreePlatform(cmd *exec.Cmd) error {
	return runTaskkill(cmd.Process.Pid, false)
}

// killProcessTreePlatform 强杀：taskkill /PID <pid> /T /F 强制终止目标进程及其子进程。
func killProcessTreePlatform(cmd *exec.Cmd) error {
	return runTaskkill(cmd.Process.Pid, true)
}

// runTaskkill 执行 taskkill /PID <pid> /T [/F]。
func runTaskkill(pid int, force bool) error {
	args := []string{"/PID", strconv.Itoa(pid), "/T"}
	if force {
		args = append(args, "/F")
	}
	out, err := exec.Command("taskkill", args...).CombinedOutput()
	if err != nil {
		// 进程已退出时 taskkill 返回非零（"not found"）——对 Stop 语义无害，不视为失败
		return fmt.Errorf("taskkill %v: %v: %s", args, err, out)
	}
	return nil
}
