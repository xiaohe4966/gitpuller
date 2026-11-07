package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 拉取状态
const (
	StatusNever   = "never"   // 尚未运行
	StatusRunning = "running" // 正在拉取
	StatusLatest  = "latest"  // 已是最新
	StatusSuccess = "success" // 拉取成功且有更新
	StatusFail    = "fail"    // 失败
)

const (
	minInterval = 10 // 最小间隔（秒），防止把仓库和网络打爆
	gitTimeout  = 180 * time.Second
)

// LogLine 一条拉取日志
type LogLine struct {
	TS   string `json:"ts"`
	Text string `json:"text"`
	Kind string `json:"kind"` // info | out | ok | err
}

// Project 一个被监控的 Git 仓库目录
type Project struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Interval int    `json:"interval"` // 秒
	Enabled  bool   `json:"enabled"`
	Strategy string `json:"strategy"` // ff-only | rebase | merge

	Branch     string `json:"branch"`
	Remote     string `json:"remote"`
	LastRun    string `json:"lastRun"`
	LastStatus string `json:"lastStatus"`
	LastMsg    string `json:"lastMsg"`
	NewCommits int    `json:"newCommits"`
	TotalRuns  int    `json:"totalRuns"`
	FailRuns   int    `json:"failRuns"`

	// 运行时字段，不落盘
	busy    bool
	nextDue time.Time
	logs    []LogLine
}

func (p *Project) clone() *Project {
	if p == nil {
		return nil
	}
	c := *p
	c.logs = append([]LogLine(nil), p.logs...)
	return &c
}

// Service 全局状态与调度器
type Service struct {
	mu       sync.RWMutex
	subMu    sync.RWMutex
	cfgPath  string
	runnerOn bool
	projects []*Project
	subs     map[chan []byte]struct{}
}

type configFile struct {
	RunnerOn bool       `json:"runnerOn"`
	Projects []*Project `json:"projects"`
}

func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "gitpuller.json"
	}
	return filepath.Join(home, ".gitpuller", "config.json")
}

func NewService(cfgPath string) (*Service, error) {
	s := &Service{
		cfgPath: cfgPath,
		subs:    make(map[chan []byte]struct{}),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	now := time.Now()
	// 恢复运行时状态：上次运行结果保留，但重置调度时间
	for _, p := range s.projects {
		if p.Interval < minInterval {
			p.Interval = 60
		}
		p.nextDue = now
		if p.LastStatus == StatusRunning || p.LastStatus == "" {
			p.LastStatus = StatusNever
			p.LastMsg = "等待首次拉取"
		}
	}
	return s, nil
}

func (s *Service) load() error {
	b, err := os.ReadFile(s.cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var cfg configFile
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("配置文件解析失败 %s: %w", s.cfgPath, err)
	}
	s.runnerOn = cfg.RunnerOn
	s.projects = cfg.Projects
	return nil
}

// save 原子写入配置
func (s *Service) save() {
	s.mu.RLock()
	cfg := configFile{RunnerOn: s.runnerOn, Projects: make([]*Project, 0, len(s.projects))}
	for _, p := range s.projects {
		cfg.Projects = append(cfg.Projects, p.clone())
	}
	s.mu.RUnlock()

	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.cfgPath), 0o755); err != nil {
		logf("创建配置目录失败: %v", err)
		return
	}
	tmp := s.cfgPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		logf("写入配置失败: %v", err)
		return
	}
	if err := os.Rename(tmp, s.cfgPath); err != nil {
		logf("替换配置失败: %v", err)
	}
}

// ---------- 订阅（SSE） ----------

func (s *Service) Subscribe() chan []byte {
	ch := make(chan []byte, 64)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	return ch
}

func (s *Service) Unsubscribe(ch chan []byte) {
	s.subMu.Lock()
	delete(s.subs, ch)
	s.subMu.Unlock()
}

func (s *Service) push(ev Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	s.subMu.RLock()
	defer s.subMu.RUnlock()
	for ch := range s.subs {
		select {
		case ch <- b:
		default: // 订阅者太慢就丢弃这一条，不阻塞拉取
		}
	}
}

type Event struct {
	Type      string        `json:"type"` // state | log | toast
	ProjectID string        `json:"projectId,omitempty"`
	Log       *LogLine      `json:"log,omitempty"`
	State     *statePayload `json:"state,omitempty"`
	Message   string        `json:"message,omitempty"`
	Kind      string        `json:"kind,omitempty"`
}

type statePayload struct {
	RunnerOn bool       `json:"runnerOn"`
	Projects []*Project `json:"projects"`
	Now      string     `json:"now"`
	Version  string     `json:"version"`
}

func (s *Service) Snapshot() statePayload {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := statePayload{
		RunnerOn: s.runnerOn,
		Now:      time.Now().Format("2006-01-02 15:04:05"),
		Version:  appVersion,
		Projects: make([]*Project, 0, len(s.projects)),
	}
	for _, p := range s.projects {
		out.Projects = append(out.Projects, p.clone())
	}
	sort.SliceStable(out.Projects, func(i, j int) bool { return out.Projects[i].Name < out.Projects[j].Name })
	return out
}

func (s *Service) Broadcast() {
	st := s.Snapshot()
	s.push(Event{Type: "state", State: &st})
}

func (s *Service) toast(msg, kind string) {
	s.push(Event{Type: "toast", Message: msg, Kind: kind})
}

// ---------- 项目操作 ----------

func (s *Service) find(id string) *Project {
	for _, p := range s.projects {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Add 添加项目，会校验目录确实是 Git 工作区
func (s *Service) Add(rawPath string, interval int, strategy string) (*Project, error) {
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" {
		return nil, errors.New("目录路径不能为空")
	}
	if strings.HasPrefix(rawPath, "~/") {
		home, _ := os.UserHomeDir()
		rawPath = filepath.Join(home, rawPath[2:])
	}
	abs, err := filepath.Abs(rawPath)
	if err != nil {
		return nil, fmt.Errorf("路径解析失败: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("目录不存在或不可访问: %s", abs)
	}
	if !info.IsDir() {
		return nil, errors.New("请选择目录，而不是文件")
	}

	out, err := runGit(abs, 10*time.Second, "rev-parse", "--is-inside-work-tree")
	if err != nil || !strings.Contains(strings.ToLower(out), "true") {
		return nil, fmt.Errorf("这个目录不是 Git 工作区（没有 .git）: %s", abs)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.projects {
		if p.Path == abs {
			return nil, fmt.Errorf("该目录已在列表中: %s", abs)
		}
	}

	if interval < minInterval {
		interval = 60
	}
	switch strategy {
	case "ff-only", "rebase", "merge":
	default:
		strategy = "ff-only"
	}

	branch, remote := readRef(abs)
	p := &Project{
		ID:         newID(),
		Name:       filepath.Base(abs),
		Path:       abs,
		Interval:   interval,
		Enabled:    true,
		Strategy:   strategy,
		Branch:     branch,
		Remote:     remote,
		LastStatus: StatusNever,
		LastMsg:    "等待首次拉取",
		nextDue:    time.Now(),
	}
	s.projects = append(s.projects, p)
	cp := p.clone()
	go s.Broadcast()
	return cp, nil
}

func (s *Service) Remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.projects {
		if p.ID == id {
			if p.busy {
				return false
			}
			s.projects = append(s.projects[:i], s.projects[i+1:]...)
			go s.Broadcast()
			return true
		}
	}
	return false
}

type patchReq struct {
	Enabled  *bool   `json:"enabled"`
	Interval *int    `json:"interval"`
	Strategy *string `json:"strategy"`
	Name     *string `json:"name"`
}

func (s *Service) Patch(id string, req patchReq) error {
	s.mu.Lock()
	p := s.find(id)
	if p == nil {
		s.mu.Unlock()
		return errors.New("项目不存在")
	}
	if req.Enabled != nil {
		p.Enabled = *req.Enabled
		if p.Enabled {
			p.nextDue = time.Now()
			if p.LastStatus == StatusNever {
				p.LastMsg = "等待首次拉取"
			}
		}
	}
	if req.Interval != nil {
		v := *req.Interval
		if v < minInterval {
			v = minInterval
		}
		p.Interval = v
		p.nextDue = time.Now()
	}
	if req.Strategy != nil {
		switch *req.Strategy {
		case "ff-only", "rebase", "merge":
			p.Strategy = *req.Strategy
		default:
			s.mu.Unlock()
			return errors.New("未知的拉取策略")
		}
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		p.Name = strings.TrimSpace(*req.Name)
	}
	s.mu.Unlock()
	s.save()
	s.Broadcast()
	return nil
}

func (s *Service) SetRunner(on bool) {
	s.mu.Lock()
	s.runnerOn = on
	if on {
		for _, p := range s.projects {
			p.nextDue = time.Now()
		}
	}
	s.mu.Unlock()
	s.save()
	s.Broadcast()
}

func (s *Service) Logs(id string) []LogLine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.find(id)
	if p == nil {
		return nil
	}
	return append([]LogLine(nil), p.logs...)
}

// ---------- 调度 ----------

func (s *Service) StartScheduler() {
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for range t.C {
			s.mu.Lock()
			if !s.runnerOn {
				s.mu.Unlock()
				continue
			}
			now := time.Now()
			var due []string
			for _, p := range s.projects {
				if !p.Enabled || p.busy || p.Interval <= 0 {
					continue
				}
				if p.nextDue.IsZero() || !now.Before(p.nextDue) {
					p.nextDue = now.Add(time.Duration(p.Interval) * time.Second)
					due = append(due, p.ID)
				}
			}
			s.mu.Unlock()
			for _, id := range due {
				s.Pull(id, false)
			}
		}
	}()
}

// Pull 触发一次拉取（manual=true 表示用户点击）
func (s *Service) Pull(id string, manual bool) {
	s.mu.Lock()
	p := s.find(id)
	if p == nil {
		s.mu.Unlock()
		return
	}
	if p.busy {
		s.mu.Unlock()
		if manual {
			s.toast("该目录正在拉取，请稍候", "warn")
		}
		return
	}
	p.busy = true
	p.LastStatus = StatusRunning
	p.LastMsg = "正在拉取…"
	s.mu.Unlock()
	s.Broadcast()

	go s.execPull(id)
}

func (s *Service) execPull(id string) {
	s.mu.RLock()
	p := s.find(id)
	if p == nil {
		s.mu.RUnlock()
		return
	}
	path, strategy, name, interval := p.Path, p.Strategy, p.Name, p.Interval
	s.mu.RUnlock()

	defer func() {
		s.mu.Lock()
		if pp := s.find(id); pp != nil {
			pp.busy = false
			pp.nextDue = time.Now().Add(time.Duration(interval) * time.Second)
		}
		s.mu.Unlock()
		s.save()
		s.Broadcast()
	}()

	_ = name
	s.addLog(id, "▶ 开始拉取（"+strategyLabel(strategy)+"）", "info")
	beforeHead := gitTrim(path, 12*time.Second, "rev-parse", "HEAD")

	args := []string{"pull"}
	switch strategy {
	case "rebase":
		args = append(args, "--rebase")
	case "merge":
		args = append(args, "--no-rebase")
	default:
		args = append(args, "--ff-only")
	}

	out, err := runGit(path, gitTimeout, args...)
	for i, line := range splitLines(out, 80) {
		kind := "out"
		if i > 40 {
			break
		}
		s.addLog(id, line, kind)
	}

	status, msg := classify(out, err)
	newCommits := 0
	afterHead := gitTrim(path, 12*time.Second, "rev-parse", "HEAD")
	if beforeHead != "" && afterHead != "" && beforeHead != afterHead {
		if n := gitTrim(path, 12*time.Second, "rev-list", "--count", beforeHead+".."+afterHead); n != "" {
			if v, e := strconv.Atoi(n); e == nil {
				newCommits = v
			}
		}
	}
	branch, remote := readRef(path)

	s.mu.Lock()
	if pp := s.find(id); pp != nil {
		pp.LastRun = time.Now().Format("2006-01-02 15:04:05")
		pp.LastStatus = status
		pp.LastMsg = msg
		pp.NewCommits = newCommits
		pp.Branch = branch
		pp.Remote = remote
		pp.TotalRuns++
		if status == StatusFail {
			pp.FailRuns++
		}
	}
	s.mu.Unlock()

	if status == StatusFail {
		s.addLog(id, "✕ "+msg, "err")
	} else if status == StatusSuccess {
		s.addLog(id, fmt.Sprintf("✓ 已更新，拉取到 %d 个提交", newCommits), "ok")
	} else {
		s.addLog(id, "✓ 已是最新", "ok")
	}
}

func (s *Service) addLog(id, text, kind string) {
	line := LogLine{TS: time.Now().Format("15:04:05"), Text: text, Kind: kind}
	s.mu.Lock()
	if p := s.find(id); p != nil {
		p.logs = append(p.logs, line)
		if len(p.logs) > 400 {
			p.logs = p.logs[len(p.logs)-400:]
		}
	}
	s.mu.Unlock()
	s.push(Event{Type: "log", ProjectID: id, Log: &line})
}

func strategyLabel(s string) string {
	switch s {
	case "rebase":
		return "rebase"
	case "merge":
		return "merge"
	default:
		return "ff-only 快进"
	}
}

// RunOnce 拉取一轮，供 cron / launchd 使用
func (s *Service) RunOnce() int {
	s.mu.RLock()
	ids := make([]string, 0, len(s.projects))
	for _, p := range s.projects {
		if p.Enabled {
			ids = append(ids, p.ID)
		}
	}
	s.mu.RUnlock()

	failed := 0
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			s.execPull(id)
			s.mu.RLock()
			p := s.find(id)
			ok := p != nil && p.LastStatus != StatusFail
			name, msg, n := "", "", 0
			if p != nil {
				name, msg, n = p.Name, p.LastMsg, p.NewCommits
			}
			s.mu.RUnlock()
			mu.Lock()
			if !ok {
				failed++
				fmt.Printf("[失败] %-24s %s\n", name, msg)
			} else {
				fmt.Printf("[成功] %-24s %s（新提交 %d）\n", name, msg, n)
			}
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	if failed > 0 {
		return 1
	}
	return 0
}

// ---------- git 封装 ----------

func runGit(dir string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", // 绝不停在密码输入上
		"GIT_ASKPASS=/bin/true",
		"SSH_ASKPASS=/bin/true",
		"GIT_PAGER=cat",
		"PAGER=cat",
	)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("执行超时（%s）", timeout)
	}
	text := string(out)
	if err != nil {
		if text == "" {
			text = err.Error()
		}
	}
	return text, err
}

func gitTrim(dir string, timeout time.Duration, args ...string) string {
	out, err := runGit(dir, timeout, args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func readRef(dir string) (branch, remote string) {
	branch = gitTrim(dir, 12*time.Second, "rev-parse", "--abbrev-ref", "HEAD")
	remote = gitTrim(dir, 12*time.Second, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	return
}

func splitLines(s string, max int) []string {
	raw := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		l = strings.TrimRight(l, "\r")
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
		if len(out) >= max {
			break
		}
	}
	return out
}

func firstLine(s string) string {
	lines := splitLines(s, 5)
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}

// classify 把 git 输出翻译成「人话」状态
func classify(out string, err error) (string, string) {
	low := strings.ToLower(out)
	if err == nil {
		if strings.Contains(low, "already up to date") || strings.Contains(out, "已经是最新") || strings.Contains(low, "already up-to-date") {
			return StatusLatest, "已是最新，无需更新"
		}
		return StatusSuccess, "拉取成功"
	}

	switch {
	case strings.Contains(low, "执行超时") || strings.Contains(low, "deadline exceeded"):
		return StatusFail, "拉取超时，可能是网络或凭据交互卡住了"
	case strings.Contains(low, "not a git repository"):
		return StatusFail, "目录不是 Git 仓库"
	case strings.Contains(low, "no tracking information") || strings.Contains(low, "no upstream configured"):
		return StatusFail, "当前分支没有上游分支，先在终端执行 git push -u origin <branch>"
	case strings.Contains(low, "your local changes") || strings.Contains(low, "would be overwritten") ||
		strings.Contains(low, "please commit your changes or stash them") || strings.Contains(low, "uncommitted changes"):
		return StatusFail, "有未提交的本地改动，已跳过本次拉取（避免覆盖工作区）"
	case strings.Contains(low, "not possible to fast-forward") || strings.Contains(low, "have diverged") || strings.Contains(low, "diverged"):
		return StatusFail, "本地与远程已分叉，ff-only 无法快进；可把策略改成 rebase"
	case strings.Contains(low, "conflict"):
		return StatusFail, "出现合并冲突，需要手动处理"
	case strings.Contains(low, "permission denied") || strings.Contains(low, "authentication failed") ||
		strings.Contains(low, "could not read from remote") || strings.Contains(low, "host key verification failed"):
		return StatusFail, "权限或主机指纹问题，先在终端手动 git pull 一次"
	case strings.Contains(low, "could not resolve host") || strings.Contains(low, "network is unreachable") ||
		strings.Contains(low, "connection refused") || strings.Contains(low, "connection timed out"):
		return StatusFail, "网络不可达，检查网络或代理"
	case strings.Contains(low, "you have not concluded your merge") || strings.Contains(low, "rebase in progress"):
		return StatusFail, "仓库处于合并/变基中途，先手动处理完"
	case strings.Contains(low, "terminal prompts disabled"):
		return StatusFail, "需要交互式输入凭据，请先在终端手动拉取一次"
	}

	if l := firstLine(out); l != "" {
		return StatusFail, l
	}
	return StatusFail, err.Error()
}
