//go:build !windows

package execx

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// configureProcessGroup 将进程放入独立进程组（pgid == pid），
// 使 Stop/force 可以对整组发信号，从而真正终止目标进程及其子进程。
// 子进程默认继承父进程的进程组，因此除非子进程自己调用 setpgid，
// 它们都会落在这个新进程组内，kill(-pgid) 可一并覆盖。
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// terminateProcessTreePlatform 优雅终止：对进程组发 SIGTERM（非阻塞）。
func terminateProcessTreePlatform(cmd *exec.Cmd) error {
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		// 进程组可能已不存在（竞态退出）；退回单进程信号
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	return nil
}

// killProcessTreePlatform 强杀：对进程组发 SIGKILL（非阻塞）。
func killProcessTreePlatform(cmd *exec.Cmd) error {
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		// 进程组可能已不存在（竞态退出）；退回单进程强杀
		return cmd.Process.Kill()
	}
	return nil
}

// platformPidAlive 探测 pid 是否仍存活（kill -0 仅探测不发送信号）。
// 供测试轮询使用；进程组/子进程存活断言也复用该探测。
func platformPidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// platformProcessTreeKilledForTest 供测试在 Unix 上构造进程树并等待子进程出现；
// 返回 shell 命令 argv（两个后台 sleep 同组）与等待窗口期的子进程 pid 列表的读取函数。
func platformProcessTreeKilledForTest() (argv []string, childPids func(parentPid int) []int, ok bool) {
	// sh -c 'sleep 30 & sleep 30'：父 shell 立即 fork 两个 sleep 后等待，
	// 二者与 sh 同进程组（pgid == sh pid）
	argv = []string{"sh", "-c", "sleep 30 & sleep 30"}
	// /proc/<pid>/task/<pid>/children 读取子进程 pid（Linux）；其他 Unix 返回空
	childPids = func(parentPid int) []int {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", parentPid, parentPid))
		if err != nil {
			return nil
		}
		var pids []int
		for _, field := range strings.Fields(string(data)) {
			var pid int
			if _, err := fmt.Sscanf(field, "%d", &pid); err == nil && pid > 0 {
				pids = append(pids, pid)
			}
		}
		return pids
	}
	return argv, childPids, true
}
