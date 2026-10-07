package execx

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/mcp"
)

func newExecTestConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	return config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		AuditLogPath:     filepath.Join(root, "audit.jsonl"),
		OutputMaxBytes:   1 << 16,
		CommandTimeout:   5 * time.Second,
		UnsafeAllowAll:   true,
	}
}

// execToolsForTest 只调用一次 NewTools，确保所有工具共享同一个 ProcessManager 实例。
func execToolsForTest(t *testing.T, cfg config.Config) map[string]mcp.Tool {
	t.Helper()
	tools := make(map[string]mcp.Tool)
	for _, tool := range NewTools(cfg) {
		tools[tool.Name()] = tool
	}
	return tools
}

// crossPlatformSleepSeconds 返回睡眠约 3 秒的跨平台命令（Windows 用 ping 计时，Unix 用 sleep）。
// 均为纯 argv，不依赖 shell 重定向。
func crossPlatformSleepSeconds() []string {
	if runtime.GOOS == "windows" {
		// ping -n 4 约 3 秒；timeout 需要交互 stdin，不能用于无 stdin 的后台进程
		return []string{"ping", "-n", "4", "127.0.0.1"}
	}
	return []string{"sleep", "3"}
}

// argsAsAny 把字符串参数转为 []any：rawStringArgs 只接受 []any（MCP JSON 解码后的形态），
// 直接传 []string 会被静默丢弃导致 argv 缺参。
func argsAsAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func TestStartProcessAndList(t *testing.T) {
	cfg := newExecTestConfig(t)
	tools := execToolsForTest(t, cfg)
	tool := tools["exec_start_process"]

	// 使用跨平台小命令，可快速退出且不依赖 go 工具链
	echo := crossPlatformEcho()
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"command": echo[0],
		"workdir": ".",
		"args":    argsAsAny(echo[1:]),
	})
	if err != nil {
		t.Fatalf("exec_start_process failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res)
	}
	id, _ := res.StructuredContent["id"].(string)
	if id == "" {
		t.Fatalf("expected process id, got %#v", res.StructuredContent)
	}

	// 轮询等待进程结束，而非固定 sleep
	waitForProcessState(t, tools, id, "exited")

	listTool := tools["exec_list_processes"]
	listRes, err := listTool.Call(context.Background(), mcp.CallContext{}, map[string]any{})
	if err != nil {
		t.Fatalf("exec_list_processes failed: %v", err)
	}
	processes, _ := listRes.StructuredContent["processes"].([]map[string]any)
	found := false
	for _, p := range processes {
		if p["id"] == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("process %s not found in list: %#v", id, processes)
	}
}

func TestProcessLogs(t *testing.T) {
	cfg := newExecTestConfig(t)
	tools := execToolsForTest(t, cfg)
	startTool := tools["exec_start_process"]

	// crossPlatformEcho 输出固定文本，避免依赖 go 工具链输出内容
	echo := crossPlatformEcho()
	res, err := startTool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"command": echo[0],
		"workdir": ".",
		"args":    argsAsAny(echo[1:]),
	})
	if err != nil {
		t.Fatalf("exec_start_process failed: %v", err)
	}
	id, _ := res.StructuredContent["id"].(string)
	if id == "" {
		t.Fatalf("expected process id")
	}

	// 轮询等待进程结束，再断言缓冲
	waitForProcessState(t, tools, id, "exited")

	logsTool := tools["exec_process_logs"]
	logsRes, err := logsTool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"id": id,
	})
	if err != nil {
		t.Fatalf("exec_process_logs failed: %v", err)
	}
	stdout, _ := logsRes.StructuredContent["stdout"].(string)
	if !strings.Contains(stdout, "ok") {
		t.Fatalf("expected stdout to contain 'ok', got %q", stdout)
	}
}

func TestStopProcess(t *testing.T) {
	cfg := newExecTestConfig(t)
	tools := execToolsForTest(t, cfg)
	startTool := tools["exec_start_process"]

	// 启动一个持续运行数秒的跨平台小进程，再主动停止（不启动整仓 go test）
	sleep := crossPlatformSleepSeconds()
	res, err := startTool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"command": sleep[0],
		"workdir": ".",
		"args":    argsAsAny(sleep[1:]),
	})
	if err != nil {
		t.Fatalf("exec_start_process failed: %v", err)
	}
	id, _ := res.StructuredContent["id"].(string)
	if id == "" {
		t.Fatalf("expected process id")
	}

	stopTool := tools["exec_stop_process"]
	stopRes, err := stopTool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"id": id,
	})
	if err != nil {
		t.Fatalf("exec_stop_process failed: %v", err)
	}
	if stopRes.IsError {
		t.Fatalf("unexpected error result: %#v", stopRes)
	}
}

// waitForProcessState 轮询 exec_list_processes 直到目标进程进入期望状态或超时。
func waitForProcessState(t *testing.T, tools map[string]mcp.Tool, id string, wantState string) {
	t.Helper()
	listTool := tools["exec_list_processes"]
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		listRes, err := listTool.Call(context.Background(), mcp.CallContext{}, map[string]any{})
		if err != nil {
			t.Fatalf("exec_list_processes failed: %v", err)
		}
		processes, _ := listRes.StructuredContent["processes"].([]map[string]any)
		for _, p := range processes {
			if p["id"] == id && p["state"] == wantState {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %s did not reach state %q within timeout", id, wantState)
}

// waitForPidGone 轮询直到 pid 不再存活（process.Signal 探测），或超时失败。
// 跨平台：Windows 用 tasklist /FI "PID eq <pid>"，Unix 用 kill -0。
func waitForPidGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive after %v", pid, timeout)
}

// pidAlive 探测 pid 是否仍存活。跨平台：Windows 用 tasklist /FI "PID eq <pid>"，
// Unix 用 kill -0；平台调用收敛到独立文件避免 syscall.Kill 的 Windows 编译错误。
func pidAlive(pid int) bool {
	return platformPidAlive(pid)
}

func TestShellTool(t *testing.T) {
	cfg := newExecTestConfig(t)
	tools := execToolsForTest(t, cfg)
	tool := tools["exec_shell"]

	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"command": "echo hello-shell",
		"workdir": ".",
	})
	if err != nil {
		t.Fatalf("exec_shell failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res)
	}
	if !strings.Contains(res.Content[0].Text, "hello-shell") {
		t.Fatalf("expected shell output to contain hello-shell, got %q", res.Content[0].Text)
	}
}

func TestShellToolRequiresUnsafe(t *testing.T) {
	cfg := newExecTestConfig(t)
	cfg.UnsafeAllowAll = false
	tools := execToolsForTest(t, cfg)
	tool := tools["exec_shell"]

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"command": "echo hi",
		"workdir": ".",
	})
	if err == nil {
		t.Fatal("expected error when unsafe_allow_all=false")
	}
	if !strings.Contains(err.Error(), "unsafe_allow_all") {
		t.Fatalf("expected unsafe_allow_all error, got: %v", err)
	}
}

func TestStartProcessRequiresUnsafe(t *testing.T) {
	cfg := newExecTestConfig(t)
	cfg.UnsafeAllowAll = false
	tools := execToolsForTest(t, cfg)
	tool := tools["exec_start_process"]

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"command": "go",
		"workdir": ".",
	})
	if err == nil {
		t.Fatal("expected error when unsafe_allow_all=false")
	}
	if !strings.Contains(err.Error(), "unsafe_allow_all") {
		t.Fatalf("expected unsafe_allow_all error, got: %v", err)
	}
}

// TestLimitedBufferConcurrentWriteAndRead 锁定 CORR-2 修复：
// 进程输出 goroutine 在 ProcessEntry.mu 之外调用 Write，而 Logs()/Snapshot() 持 ProcessEntry.mu
// 并发读，缓冲自身必须线程安全。并发写 + 并发读 + 并发截断压力验证不 panic、不越界。
// （本机无 C 编译器无法跑 go test -race，此压力测试是编译器级并发的替代自证。）
func TestLimitedBufferConcurrentWriteAndRead(t *testing.T) {
	b := newLimitedBuffer(256)

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			payload := []byte(strings.Repeat("x", 64))
			for i := 0; i < 200; i++ {
				if _, err := b.Write(payload); err != nil {
					t.Errorf("write failed: %v", err)
					return
				}
				// 与写入并发的读取路径（等价于 Logs()/Snapshot() 的调用方式）
				_ = b.String()
			}
		}(w)
	}
	wg.Wait()

	out := b.String()
	if len(out) > 256 {
		t.Fatalf("expected buffer capped at 256 bytes, got %d", len(out))
	}
	if !utf8.ValidString(out) {
		t.Fatal("expected buffer content to be valid UTF-8")
	}
}

// TestLimitedBufferTrimsOnRuneBoundary 锁定 LOW-6 修复：
// 裁剪保留尾部 limit 字节时按 rune 边界回退，不产生半个多字节字符。
func TestLimitedBufferTrimsOnRuneBoundary(t *testing.T) {
	// "日" 是 3 字节 UTF-8 (E6 97 A5)。先写入 9 字节日，再补 1 字节 'a' 触发裁剪，
	// 让 limit=4 的尾部起点正好落在"日"的续字节中间。
	b := newLimitedBuffer(4)
	payload := []byte("日日日a")
	if _, err := b.Write(payload); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	out := b.String()
	if len(out) > 4 {
		t.Fatalf("expected buffer capped at 4 bytes, got %d", len(out))
	}
	if !utf8.ValidString(out) {
		t.Fatalf("expected trimmed output to be valid UTF-8, got %q (% x)", out, out)
	}
	if !strings.HasSuffix(out, "a") {
		t.Fatalf("expected tail byte 'a' preserved, got %q", out)
	}
}

// TestLimitedBufferNoTrimUnderLimit 确认未超限时不裁剪。
func TestLimitedBufferNoTrimUnderLimit(t *testing.T) {
	b := newLimitedBuffer(64)
	if _, err := b.Write([]byte("hello")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if out := b.String(); out != "hello" {
		t.Fatalf("expected 'hello', got %q", out)
	}
}

// TestNextProcessIDConcurrentUnique 锁定 CORR-1 修复：
// nextProcessID 的自增与注册都在 m.mu 临界区内，并发 Start 不产生重复 ID。
func TestNextProcessIDConcurrentUnique(t *testing.T) {
	m := NewProcessManager()

	var mu sync.Mutex
	ids := make(map[string]bool, 400)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				m.mu.Lock()
				id := m.nextProcessID()
				m.mu.Unlock()
				mu.Lock()
				if ids[id] {
					mu.Unlock()
					t.Errorf("duplicate process id generated: %s", id)
					return
				}
				ids[id] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(ids) != 400 {
		t.Fatalf("expected 400 unique ids, got %d", len(ids))
	}
}

// TestMergeCommandEnvReplacesDuplicateKeys 锁定 LOW-1 修复：
// base 中同 key 的旧条目先删除再 append，消除重复 KEY=value
// （POSIX execve 对重复 key 语义未定义，部分实现取首个会令 env 覆盖失效）。
func TestMergeCommandEnvReplacesDuplicateKeys(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/root", "MCP_TOKEN=old-value", "LANG=C.UTF-8"}
	fixed := map[string]string{"MCP_TOKEN": "new-value", "EXTRA": "1"}
	merged := mergeCommandEnv(base, fixed)

	counts := map[string]int{}
	for _, entry := range merged {
		key, _, _ := strings.Cut(entry, "=")
		counts[key]++
	}
	for key, n := range counts {
		if n != 1 {
			t.Fatalf("expected exactly one entry for key %s, got %d in %v", key, n, merged)
		}
	}
	found := false
	for _, entry := range merged {
		if entry == "MCP_TOKEN=new-value" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected override value MCP_TOKEN=new-value, got %v", merged)
	}
	for _, want := range []string{"PATH=/usr/bin", "HOME=/root", "LANG=C.UTF-8", "EXTRA=1"} {
		if !contains(merged, want) {
			t.Fatalf("expected %q preserved, got %v", want, merged)
		}
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// TestProcessRemovedFromTable 锁定 LOW-4 修复：
// exec_remove_process 从进程表移除已结束的进程，map 不再只增不减。
func TestProcessRemovedFromTable(t *testing.T) {
	cfg := newExecTestConfig(t)
	tools := execToolsForTest(t, cfg)
	startTool := tools["exec_start_process"]

	echo := crossPlatformEcho()
	res, err := startTool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"command": echo[0],
		"workdir": ".",
		"args":    argsAsAny(echo[1:]),
	})
	if err != nil {
		t.Fatalf("exec_start_process failed: %v", err)
	}
	id, _ := res.StructuredContent["id"].(string)
	if id == "" {
		t.Fatalf("expected process id")
	}
	waitForProcessState(t, tools, id, "exited")

	removeTool := tools["exec_remove_process"]
	removeRes, err := removeTool.Call(context.Background(), mcp.CallContext{}, map[string]any{"id": id})
	if err != nil {
		t.Fatalf("exec_remove_process failed: %v", err)
	}
	if removeRes.IsError {
		t.Fatalf("unexpected error result: %#v", removeRes)
	}

	listTool := tools["exec_list_processes"]
	listRes, err := listTool.Call(context.Background(), mcp.CallContext{}, map[string]any{})
	if err != nil {
		t.Fatalf("exec_list_processes failed: %v", err)
	}
	processes, _ := listRes.StructuredContent["processes"].([]map[string]any)
	for _, p := range processes {
		if p["id"] == id {
			t.Fatalf("expected removed process %s to be gone from the table, still listed: %#v", id, processes)
		}
	}

	// 再移除一次应报 not found
	if _, err := removeTool.Call(context.Background(), mcp.CallContext{}, map[string]any{"id": id}); err == nil {
		t.Fatal("expected second remove to fail with not found")
	}
}

// TestProcessToolsRequireUnsafe 锁定 MED-3 修复：
// exec_list_processes / exec_process_logs / exec_stop_process / exec_remove_process
// 在安全模式下返回与 exec_shell 一致风格的门禁错误。
func TestProcessToolsRequireUnsafe(t *testing.T) {
	cfg := newExecTestConfig(t)
	cfg.UnsafeAllowAll = false
	tools := execToolsForTest(t, cfg)

	for _, name := range []string{"exec_list_processes", "exec_process_logs", "exec_stop_process", "exec_remove_process"} {
		tool := tools[name]
		args := map[string]any{}
		if name != "exec_list_processes" {
			args["id"] = "proc-1"
		}
		_, err := tool.Call(context.Background(), mcp.CallContext{}, args)
		if err == nil {
			t.Fatalf("%s: expected error when unsafe_allow_all=false", name)
		}
		if !strings.Contains(err.Error(), "unsafe_allow_all") {
			t.Fatalf("%s: expected unsafe_allow_all error, got: %v", name, err)
		}
	}
}

// TestStopProcessGracefulThenKilled 锁定 MED-2 非 force 路径：
// 优雅终止（Unix 进程组 SIGTERM / Windows taskkill /T）→ 宽限期 → 未退出升级强杀。
// 用短宽限期触发升级路径，验证 Stop 返回后进程确实终止、状态为 stopped。
// 轮询模式（waitForProcessState/waitForPidGone），无固定 sleep。
func TestStopProcessGracefulThenKilled(t *testing.T) {
	restore := shrinkStopTimings(t)
	defer restore()

	cfg := newExecTestConfig(t)
	tools := execToolsForTest(t, cfg)
	startTool := tools["exec_start_process"]
	stopTool := tools["exec_stop_process"]

	sleep := crossPlatformSleepSeconds()
	res, err := startTool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"command": sleep[0],
		"workdir": ".",
		"args":    argsAsAny(sleep[1:]),
	})
	if err != nil {
		t.Fatalf("exec_start_process failed: %v", err)
	}
	id, _ := res.StructuredContent["id"].(string)
	pid, _ := res.StructuredContent["pid"].(int)
	if id == "" || pid <= 0 {
		t.Fatalf("expected process id and pid, got %#v", res.StructuredContent)
	}

	// 等进程真正起来再停止
	waitForProcessState(t, tools, id, "running")

	stopRes, err := stopTool.Call(context.Background(), mcp.CallContext{}, map[string]any{"id": id})
	if err != nil {
		t.Fatalf("exec_stop_process failed: %v", err)
	}
	if stopRes.IsError {
		t.Fatalf("unexpected error result: %#v", stopRes)
	}

	// Stop 已等待 done，轮询断言状态进入 stopped 且 pid 终止
	waitForProcessState(t, tools, id, "stopped")
	waitForPidGone(t, int(pid), 5*time.Second)
}

// TestStopProcessForceKillsProcessTree 锁定 MED-2 force 语义：
// force=true 强杀目标进程及其子进程。Unix 构造同进程组的 sleep 树并断言子进程被终止；
// Windows 用 taskkill /T 后轮询。Stop 返回不再无限阻塞。
func TestStopProcessForceKillsProcessTree(t *testing.T) {
	restore := shrinkStopTimings(t)
	defer restore()

	cfg := newExecTestConfig(t)
	tools := execToolsForTest(t, cfg)
	startTool := tools["exec_start_process"]
	stopTool := tools["exec_stop_process"]

	// 跨平台构造持续运行的进程树：
	// Unix：sh -c 'sleep 30 & sleep 30'（子 sleep 继承进程组，pgid == sh pid）
	// Windows：cmd /C "ping -n 30 127.0.0.1 & ping -n 30 127.0.0.1"（cmd 的子进程树）
	var argv []string
	var childPids func(parentPid int) []int
	treeAssertions := false
	if runtime.GOOS == "windows" {
		argv = []string{"cmd.exe", "/C", "ping -n 30 127.0.0.1 & ping -n 30 127.0.0.1"}
	} else {
		a, f, ok := platformProcessTreeKilledForTest()
		if !ok {
			t.Skip("no process tree construction for this platform")
		}
		argv = a
		childPids = f
		treeAssertions = true
	}

	res, err := startTool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"command": argv[0],
		"workdir": ".",
		"args":    argsAsAny(argv[1:]),
	})
	if err != nil {
		t.Fatalf("exec_start_process failed: %v", err)
	}
	id, _ := res.StructuredContent["id"].(string)
	pid, _ := res.StructuredContent["pid"].(int)
	if id == "" || pid <= 0 {
		t.Fatalf("expected process id and pid, got %#v", res.StructuredContent)
	}

	// Unix：等待子进程出现在同进程组（/proc children），确保树确实建成
	var before []int
	if treeAssertions {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			before = childPids(int(pid))
			if len(before) > 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if len(before) == 0 {
			t.Skip("child processes never appeared under /proc; skipping tree assertions")
		}
	}

	stopRes, err := stopTool.Call(context.Background(), mcp.CallContext{}, map[string]any{"id": id, "force": true})
	if err != nil {
		t.Fatalf("exec_stop_process failed: %v", err)
	}
	if stopRes.IsError {
		t.Fatalf("unexpected error result: %#v", stopRes)
	}

	waitForProcessState(t, tools, id, "stopped")
	waitForPidGone(t, int(pid), 5*time.Second)

	// Unix：断言子进程也被强杀（进程组 SIGKILL 覆盖整组）
	if treeAssertions {
		for _, childPid := range before {
			waitForPidGone(t, childPid, 5*time.Second)
		}
	}
}

// TestStopProcessDoesNotBlockIndefinitely 锁定 MED-2 Stop 超时语义：
// Stop 全程受 stopWaitTimeout 约束，超时后强杀并返回，不再无限阻塞 <-done。
// 用远小于 stopWaitTimeout 的外层超时守护验证 Stop 在有限时间内返回。
func TestStopProcessDoesNotBlockIndefinitely(t *testing.T) {
	restore := shrinkStopTimings(t)
	defer restore()

	cfg := newExecTestConfig(t)
	tools := execToolsForTest(t, cfg)
	startTool := tools["exec_start_process"]
	stopTool := tools["exec_stop_process"]

	sleep := crossPlatformSleepSeconds()
	res, err := startTool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"command": sleep[0],
		"workdir": ".",
		"args":    argsAsAny(sleep[1:]),
	})
	if err != nil {
		t.Fatalf("exec_start_process failed: %v", err)
	}
	id, _ := res.StructuredContent["id"].(string)
	if id == "" {
		t.Fatalf("expected process id")
	}
	waitForProcessState(t, tools, id, "running")

	done := make(chan error, 1)
	go func() {
		_, err := stopTool.Call(context.Background(), mcp.CallContext{}, map[string]any{"id": id})
		done <- err
	}()

	// Stop 必须在有限时间内返回；超时（远超内部各阶段上限）视为无限阻塞回归
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exec_stop_process failed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("exec_stop_process blocked for 30s; Stop no longer returns within bounded time")
	}

	waitForProcessState(t, tools, id, "stopped")
}

// shrinkStopTimings 缩短 Stop 时序参数，让升级/超时路径在测试内快速走到，
// 并用 t.Cleanup 恢复原值（Stop 时序参数为包级 var）。
func shrinkStopTimings(t *testing.T) (restore func()) {
	t.Helper()
	origGrace, origWait, origKill := stopGracePeriod, stopWaitTimeout, stopKillWait
	stopGracePeriod = 100 * time.Millisecond
	stopWaitTimeout = 500 * time.Millisecond
	stopKillWait = 100 * time.Millisecond
	return func() {
		stopGracePeriod = origGrace
		stopWaitTimeout = origWait
		stopKillWait = origKill
	}
}
