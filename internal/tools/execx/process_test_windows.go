//go:build windows

package execx

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// platformPidAlive 探测 pid 是否仍存活：tasklist /FI "PID eq <pid>" 命中时输出含映像表，
// 未命中输出 ASCII "INFO: No tasks are running which match the specified criteria."。
// 供测试轮询使用；Windows 无 kill -0，用 tasklist 等效探测。
func platformPidAlive(pid int) bool {
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid)).CombinedOutput()
	if err != nil {
		return false
	}
	alive := !strings.Contains(strings.ToUpper(string(out)), "INFO: NO TASKS")
	if testing.Verbose() && alive {
		fmt.Printf("platformPidAlive(%d): output=%q\n", pid, string(out))
	}
	return alive
}

// platformProcessTreeKilledForTest 在 Windows 上不提供进程树断言通道：
// 无 /proc children 读取，进程树断言由调用方按 runtime.GOOS 跳过
// （force 杀树断言至少在 Unix 验证，Windows 用 taskkill 后轮询主进程终止）。
func platformProcessTreeKilledForTest() (argv []string, childPids func(parentPid int) []int, ok bool) {
	return nil, nil, false
}
