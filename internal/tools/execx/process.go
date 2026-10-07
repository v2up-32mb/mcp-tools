package execx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/mcp"
	"github.com/example/mcp-tools/internal/security"
	"github.com/example/mcp-tools/internal/util"
)

// ProcessState 描述一个后台进程的运行状态。
type ProcessState string

const (
	ProcessRunning ProcessState = "running"
	ProcessExited  ProcessState = "exited"
	ProcessStopped ProcessState = "stopped"
)

// Stop 终止时序参数：
//   - stopGracePeriod：非 force 优雅终止（Unix 进程组 SIGTERM / Windows taskkill /T）后的宽限期，
//     超时升级为强杀进程树；
//   - stopWaitTimeout：Stop 等待进程退出（done）的总超时，超时后强杀进程树并返回，不再无限阻塞；
//   - stopKillWait：兜底强杀后的短暂收尾窗口，仍未退出也直接返回。
//
// 用 var（而非 const）便于同包测试缩短宽限期以验证升级路径。
var (
	stopGracePeriod = 5 * time.Second
	stopWaitTimeout = 10 * time.Second
	stopKillWait    = 2 * time.Second
)

// ProcessEntry 记录一个后台进程的元信息与输出缓冲。
type ProcessEntry struct {
	ID        string
	Command   []string
	Workdir   string
	StartedAt time.Time
	State     ProcessState
	ExitCode  int
	Pid       int

	mu       sync.Mutex
	stdout   *limitedBuffer
	stderr   *limitedBuffer
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
}

// limitedBuffer 是带容量上限的追加缓冲，超出部分按 rune 边界裁剪保留尾部。
// 内嵌 mutex：进程输出 goroutine 会在 ProcessEntry.mu 之外直接调用 Write（cmd.Stdout/Stderr），
// 而 Logs()/Snapshot() 持 ProcessEntry.mu 并发读取，因此缓冲自身的所有访问都必须持锁，
// 否则构成 data race。
type limitedBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func newLimitedBuffer(limit int) *limitedBuffer {
	if limit <= 0 {
		limit = 1 << 20
	}
	return &limitedBuffer{limit: limit}
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > b.limit {
		// 保留尾部 limit 字节，避免无界增长
		keep := b.data[len(b.data)-b.limit:]
		// 按 rune 边界回退：若尾部起点落在被切断的多字节字符中间，
		// 跳过其续字节（最多 3 个），避免 process_logs 返回半个 UTF-8 字符。
		start := 0
		for start < len(keep) && start < utf8.UTFMax-1 && keep[start]&0xC0 == 0x80 {
			start++
		}
		b.data = append(b.data[:0], keep[start:]...)
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// ProcessManager 管理所有后台进程。
type ProcessManager struct {
	mu        sync.Mutex
	processes map[string]*ProcessEntry
	nextID    int
}

func NewProcessManager() *ProcessManager {
	return &ProcessManager{
		processes: make(map[string]*ProcessEntry),
		nextID:    1,
	}
}

func (m *ProcessManager) nextProcessID() string {
	// 必须在 m.mu 临界区内调用；并发 Start 下保证 ID 唯一。
	m.nextID++
	return fmt.Sprintf("proc-%d", m.nextID-1)
}

// Start 启动一个后台进程并立即返回。
func (m *ProcessManager) Start(cfg config.Config, argv []string, workdir string, env map[string]string) (*ProcessEntry, error) {
	resolvedWorkdir, err := security.RequireExistingWorkdir(workdir)
	if err != nil {
		return nil, fmt.Errorf("workdir: %w", err)
	}
	if err := ensureManagedEnvDirs(env); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	// 在 m.mu 临界区内完成 ID 分配与注册，确保并发 Start 不产生重复 ID。
	m.mu.Lock()
	entry := &ProcessEntry{
		ID:        m.nextProcessID(),
		Command:   util.CloneStrings(argv),
		Workdir:   resolvedWorkdir,
		StartedAt: time.Now(),
		State:     ProcessRunning,
		Pid:       -1,
		stdout:    newLimitedBuffer(cfg.OutputMaxBytes),
		stderr:    newLimitedBuffer(cfg.OutputMaxBytes),
		done:      make(chan struct{}),
	}
	entry.cancel = cancel
	m.processes[entry.ID] = entry
	m.mu.Unlock()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = resolvedWorkdir
	cmd.Env = mergeCommandEnv(os.Environ(), env)
	cmd.Stdout = entry.stdout
	cmd.Stderr = entry.stderr
	// Unix：放入独立进程组（pgid == pid），Stop/force 才能对整组发信号终止进程树（见 process_unix.go）；
	// Windows 为 no-op，进程树终止交给 taskkill /T（见 process_windows.go）。
	configureProcessGroup(cmd)
	// entry.cmd / entry.Pid 与 Stop、Snapshot 的读取并发，读写均持 entry.mu
	entry.mu.Lock()
	entry.cmd = cmd
	entry.mu.Unlock()

	if err := cmd.Start(); err != nil {
		cancel()
		m.mu.Lock()
		delete(m.processes, entry.ID)
		m.mu.Unlock()
		return nil, fmt.Errorf("start process: %w", err)
	}
	entry.mu.Lock()
	entry.Pid = cmd.Process.Pid
	entry.mu.Unlock()

	go func() {
		err := cmd.Wait()
		entry.mu.Lock()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				entry.ExitCode = exitErr.ExitCode()
				entry.State = ProcessExited
			} else {
				entry.State = ProcessStopped
			}
		} else {
			entry.State = ProcessExited
			entry.ExitCode = 0
		}
		entry.mu.Unlock()
		close(entry.done)
	}()

	return entry, nil
}

// Get 返回指定 ID 的进程条目；不存在时返回 nil。
func (m *ProcessManager) Get(id string) *ProcessEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.processes[id]
}

// List 返回按 ID 排序的进程快照。
func (m *ProcessManager) List() []*ProcessEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*ProcessEntry, 0, len(m.processes))
	for _, entry := range m.processes {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Stop 停止指定进程，时序：终止动作 → 有限等待 done → 未退出则升级强杀进程树 → 短暂收尾。
// 不再无限阻塞 <-entry.done（遗留项 MED-2）。
//
// 非 force：优雅终止（Unix 进程组 SIGTERM / Windows taskkill /T），宽限期内未退出则升级强杀；
// force：直接强杀目标进程及其子进程（Windows taskkill /T /F；Unix 进程组 SIGKILL）。
// 全程有 stopWaitTimeout 总超时兜底，任何分支最终都会返回。
func (m *ProcessManager) Stop(id string, force bool) (*ProcessEntry, error) {
	entry := m.Get(id)
	if entry == nil {
		return nil, fmt.Errorf("process %q not found", id)
	}
	entry.stopOnce.Do(func() {
		if force {
			// force：直接强杀目标进程及其子进程，不经过宽限期
			_ = killProcessTree(entry)
			entry.cancel()
		} else {
			// 非 force：先优雅终止（SIGTERM 等效机制 / Windows taskkill /T）
			_ = terminateProcessTree(entry)
			entry.cancel()
			// 宽限期内未退出，升级为强杀进程树
			select {
			case <-entry.done:
			case <-time.After(stopGracePeriod):
				_ = killProcessTree(entry)
			}
		}
		// 等待 done 加总超时：超时后强杀并返回，不再无限阻塞（避免子进程持管道时挂起）
		select {
		case <-entry.done:
		case <-time.After(stopWaitTimeout):
			_ = killProcessTree(entry)
			select {
			case <-entry.done:
			case <-time.After(stopKillWait):
				// 收尾窗口过后仍存活也直接返回；进程表状态按 running 记录，由后续 Stop/Remove 收尾
			}
		}
		entry.mu.Lock()
		entry.State = ProcessStopped
		entry.mu.Unlock()
	})
	return entry, nil
}

// Remove 从管理器移除已结束的进程记录。
func (m *ProcessManager) Remove(id string) error {
	entry := m.Get(id)
	if entry == nil {
		return fmt.Errorf("process %q not found", id)
	}
	entry.mu.Lock()
	state := entry.State
	entry.mu.Unlock()
	if state == ProcessRunning {
		return fmt.Errorf("process %q still running", id)
	}
	m.mu.Lock()
	delete(m.processes, id)
	m.mu.Unlock()
	return nil
}

// Snapshot 返回进程的可序列化摘要。
func (p *ProcessEntry) Snapshot() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return map[string]any{
		"id":         p.ID,
		"command":    p.Command,
		"workdir":    p.Workdir,
		"started_at": p.StartedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		"state":      string(p.State),
		"exit_code":  p.ExitCode,
		"pid":        p.Pid,
		"stdout":     p.stdout.String(),
		"stderr":     p.stderr.String(),
		"running":    p.State == ProcessRunning,
	}
}

// Logs 返回进程的 stdout/stderr 缓冲。
func (p *ProcessEntry) Logs() (string, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stdout.String(), p.stderr.String()
}

// Wait 阻塞直到进程结束或 ctx 取消。
func (p *ProcessEntry) Wait(ctx context.Context) error {
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// configureProcessGroup 在启动前为命令设置平台进程组属性（平台分支实现见 process_unix.go / process_windows.go）。
// 在 process.go 中声明签名供包内调用；具体实现按 GOOS 分别提供。
func killProcessTree(entry *ProcessEntry) error {
	if entry == nil {
		return nil
	}
	entry.mu.Lock()
	cmd := entry.cmd
	entry.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return killProcessTreePlatform(cmd)
}

// terminateProcessTree 优雅终止目标进程及其子进程：
// Unix 对进程组发 SIGTERM（子进程继承进程组）；Windows 用 taskkill /PID <pid> /T（不带 /F）。
func terminateProcessTree(entry *ProcessEntry) error {
	if entry == nil {
		return nil
	}
	entry.mu.Lock()
	cmd := entry.cmd
	entry.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return terminateProcessTreePlatform(cmd)
}

// --- 工具定义 ---

type startProcessTool struct {
	cfg config.Config
	mgr *ProcessManager
}

type listProcessesTool struct {
	cfg config.Config
	mgr *ProcessManager
}

type processLogsTool struct {
	cfg config.Config
	mgr *ProcessManager
}

type stopProcessTool struct {
	cfg config.Config
	mgr *ProcessManager
}

type removeProcessTool struct {
	cfg config.Config
	mgr *ProcessManager
}

func (startProcessTool) Name() string { return "exec_start_process" }
func (startProcessTool) Description() string {
	return "Start a background process asynchronously and return immediately with a process ID. Only available when unsafe_allow_all is enabled. The process keeps running after the call returns; use exec_list_processes / exec_process_logs / exec_stop_process to manage it."
}
func (startProcessTool) ReadOnly() bool { return false }
func (startProcessTool) Schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"command": map[string]any{"type": "string", "minLength": 1},
			"workdir": map[string]any{"type": "string", "minLength": 1},
			"args":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"env":     map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
		},
		"required": []string{"command", "workdir"},
	}
}

func (t startProcessTool) Call(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
	if !t.cfg.UnsafeAllowAll {
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("exec_start_process requires unsafe_allow_all=true"), mcp.AuditData{Allowed: true, ResultDigest: "feature disabled"})
	}
	command, ok := args["command"].(string)
	if !ok || strings.TrimSpace(command) == "" {
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("command required"), mcp.AuditData{Allowed: true, ResultDigest: "validation failed"})
	}
	workdir, ok := args["workdir"].(string)
	if !ok || strings.TrimSpace(workdir) == "" {
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("workdir required"), mcp.AuditData{Allowed: true, ResultDigest: "validation failed"})
	}
	rawArgs, err := rawStringArgs(args["args"])
	if err != nil {
		return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Workdir: workdir, Allowed: true, ResultDigest: "validation failed"})
	}
	env, err := rawEnv(args["env"])
	if err != nil {
		return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Workdir: workdir, Allowed: true, ResultDigest: "validation failed"})
	}
	absoluteWorkdir := workdir
	if !filepath.IsAbs(absoluteWorkdir) {
		absoluteWorkdir = filepath.Join(t.cfg.StartupDirectory, absoluteWorkdir)
	}
	argv := append([]string{command}, rawArgs...)
	entry, err := t.mgr.Start(t.cfg, argv, absoluteWorkdir, env)
	if err != nil {
		return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Workdir: workdir, Allowed: true, ResultDigest: "start failed"})
	}
	summary := fmt.Sprintf("started process %s (pid %d)", entry.ID, entry.Pid)
	return mcp.TextResult(summary, map[string]any{
		"summary": summary,
		"id":      entry.ID,
		"pid":     entry.Pid,
		"command": argv,
		"workdir": absoluteWorkdir,
	}, mcp.AuditData{Workdir: absoluteWorkdir, Allowed: true, ResultDigest: summary}), nil
}

func (listProcessesTool) Name() string { return "exec_list_processes" }
func (listProcessesTool) Description() string {
	return "List all background processes started via exec_start_process, including their state, pid, and captured output. Only available when unsafe_allow_all is enabled."
}
func (listProcessesTool) ReadOnly() bool { return true }
func (listProcessesTool) Schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           map[string]any{},
	}
}

func (t listProcessesTool) Call(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
	if !t.cfg.UnsafeAllowAll {
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("exec_list_processes requires unsafe_allow_all=true"), mcp.AuditData{Allowed: true, ResultDigest: "feature disabled"})
	}
	entries := t.mgr.List()
	snapshots := make([]map[string]any, 0, len(entries))
	rows := make([]string, 0, len(entries))
	for _, entry := range entries {
		snap := entry.Snapshot()
		snapshots = append(snapshots, snap)
		rows = append(rows, fmt.Sprintf("%s\t%s\tpid=%v\texit=%d\t%s", snap["id"], snap["state"], snap["pid"], snap["exit_code"], strings.Join(entry.Command, " ")))
	}
	summary := fmt.Sprintf("%d background process(es)", len(entries))
	text := summary
	if len(rows) > 0 {
		text = strings.Join(rows, "\n")
	}
	return mcp.TextResult(text, map[string]any{
		"summary":   summary,
		"processes": snapshots,
	}, mcp.AuditData{Allowed: true, ResultDigest: summary}), nil
}

func (processLogsTool) Name() string { return "exec_process_logs" }
func (processLogsTool) Description() string {
	return "Fetch the captured stdout/stderr of a background process started via exec_start_process. Only available when unsafe_allow_all is enabled."
}
func (processLogsTool) ReadOnly() bool { return true }
func (processLogsTool) Schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "minLength": 1},
		},
		"required": []string{"id"},
	}
}

func (t processLogsTool) Call(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
	if !t.cfg.UnsafeAllowAll {
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("exec_process_logs requires unsafe_allow_all=true"), mcp.AuditData{Allowed: true, ResultDigest: "feature disabled"})
	}
	id, ok := args["id"].(string)
	if !ok || strings.TrimSpace(id) == "" {
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("id required"), mcp.AuditData{Allowed: true, ResultDigest: "validation failed"})
	}
	entry := t.mgr.Get(id)
	if entry == nil {
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("process %q not found", id), mcp.AuditData{Allowed: true, ResultDigest: "process not found"})
	}
	stdout, stderr := entry.Logs()
	snap := entry.Snapshot()
	summary := fmt.Sprintf("logs for %s (state=%s)", id, snap["state"])
	return mcp.TextResult(summary, map[string]any{
		"summary": summary,
		"id":      id,
		"state":   snap["state"],
		"stdout":  stdout,
		"stderr":  stderr,
	}, mcp.AuditData{Allowed: true, ResultDigest: summary}), nil
}

func (stopProcessTool) Name() string { return "exec_stop_process" }
func (stopProcessTool) Description() string {
	return "Stop a background process started via exec_start_process. force=true also attempts to kill child processes. Only available when unsafe_allow_all is enabled."
}
func (stopProcessTool) ReadOnly() bool { return false }
func (stopProcessTool) Schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"id":    map[string]any{"type": "string", "minLength": 1},
			"force": map[string]any{"type": "boolean"},
		},
		"required": []string{"id"},
	}
}

func (t stopProcessTool) Call(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
	if !t.cfg.UnsafeAllowAll {
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("exec_stop_process requires unsafe_allow_all=true"), mcp.AuditData{Allowed: true, ResultDigest: "feature disabled"})
	}
	id, ok := args["id"].(string)
	if !ok || strings.TrimSpace(id) == "" {
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("id required"), mcp.AuditData{Allowed: true, ResultDigest: "validation failed"})
	}
	force, _ := args["force"].(bool)
	entry, err := t.mgr.Stop(id, force)
	if err != nil {
		return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Allowed: true, ResultDigest: "stop failed"})
	}
	summary := fmt.Sprintf("stopped process %s", id)
	return mcp.TextResult(summary, map[string]any{
		"summary": summary,
		"id":      id,
		"force":   force,
		"workdir": entry.Workdir,
	}, mcp.AuditData{Workdir: entry.Workdir, Allowed: true, ResultDigest: summary}), nil
}

func (removeProcessTool) Name() string { return "exec_remove_process" }
func (removeProcessTool) Description() string {
	return "Remove a finished (exited or stopped) background process from the process table so it no longer appears in exec_list_processes. Only available when unsafe_allow_all is enabled. Running processes cannot be removed."
}
func (removeProcessTool) ReadOnly() bool { return false }
func (removeProcessTool) Schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "minLength": 1},
		},
		"required": []string{"id"},
	}
}

func (t removeProcessTool) Call(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
	if !t.cfg.UnsafeAllowAll {
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("exec_remove_process requires unsafe_allow_all=true"), mcp.AuditData{Allowed: true, ResultDigest: "feature disabled"})
	}
	id, ok := args["id"].(string)
	if !ok || strings.TrimSpace(id) == "" {
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("id required"), mcp.AuditData{Allowed: true, ResultDigest: "validation failed"})
	}
	if err := t.mgr.Remove(id); err != nil {
		digest := "remove failed"
		if strings.Contains(err.Error(), "still running") {
			digest = "process still running"
		}
		return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Allowed: true, ResultDigest: digest})
	}
	summary := fmt.Sprintf("removed process %s", id)
	return mcp.TextResult(summary, map[string]any{
		"summary": summary,
		"id":      id,
	}, mcp.AuditData{Allowed: true, ResultDigest: summary}), nil
}

// drainCopy 将 reader 内容写入 writer（保留给流式输出扩展使用）。
func drainCopy(dst io.Writer, src io.Reader) {
	_, _ = io.Copy(dst, src)
}
