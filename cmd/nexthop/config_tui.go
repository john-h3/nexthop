package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"nexthop/internal/config"
	"nexthop/internal/control"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// cmdConfig 进入 TUI 配置编辑器：方向键导航、回车编辑全局配置与上游、
// a 新增 / d 删除上游、s 保存并热加载、q 退出。
func cmdConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	sockPath := fs.String("s", defaultSockPath, "control unix socket 路径")
	fs.StringVar(sockPath, "socket", defaultSockPath, "control unix socket 路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c := control.NewClient(*sockPath)
	e, err := newCfgEditor(c)
	if err != nil {
		return err
	}
	return e.run()
}

// newCfgEditor 从 daemon 读取当前配置并构造配置编辑器（主菜单复用）。
func newCfgEditor(c *control.Client) (*cfgEditor, error) {
	data, err := c.GetConfig()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("daemon 返回的配置无法解析: %w", err)
	}
	return &cfgEditor{cfg: cfg, c: c}, nil
}

// ---------- 按键事件 ----------

type keyAct int

const (
	actNone keyAct = iota
	actChar
	actEnter
	actEsc
	actUp
	actDown
	actBackspace
)

type keyEvent struct {
	act keyAct
	ch  byte
}

// ---------- 全局配置字段 ----------

type cfgField struct {
	label string
	get   func(*config.Config) string
	apply func(*config.Config, string) error
}

func fieldDur(label string, get func(*config.Config) time.Duration, set func(*config.Config) *time.Duration) cfgField {
	return cfgField{
		label: label,
		get:   func(c *config.Config) string { return get(c).String() },
		apply: func(c *config.Config, s string) error {
			d, err := time.ParseDuration(strings.TrimSpace(s))
			if err != nil {
				return fmt.Errorf("无效时长（支持 1ms/1s/1m）: %v", err)
			}
			*set(c) = d
			return nil
		},
	}
}

func fieldInt(label string, get func(*config.Config) int, set func(*config.Config) *int, min int) cfgField {
	return cfgField{
		label: label,
		get:   func(c *config.Config) string { return strconv.Itoa(get(c)) },
		apply: func(c *config.Config, s string) error {
			n, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil {
				return fmt.Errorf("%q 不是整数", s)
			}
			if n < min {
				return fmt.Errorf("必须 >= %d", min)
			}
			*set(c) = n
			return nil
		},
	}
}

func fieldStr(label string, get func(*config.Config) string, set func(*config.Config) *string, validate func(string) error) cfgField {
	return cfgField{
		label: label,
		get:   func(c *config.Config) string { return get(c) },
		apply: func(c *config.Config, s string) error {
			s = strings.TrimSpace(s)
			if err := validate(s); err != nil {
				return err
			}
			*set(c) = s
			return nil
		},
	}
}

func validateNonEmpty(s string) error {
	if s == "" {
		return fmt.Errorf("不能为空")
	}
	return nil
}

func validateIPv4(s string) error {
	ip := net.ParseIP(s)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("%q 不是合法的 IPv4 地址", s)
	}
	return nil
}

var globalFields = []cfgField{
	fieldDur("probe_interval", func(c *config.Config) time.Duration { return c.ProbeInterval }, func(c *config.Config) *time.Duration { return &c.ProbeInterval }),
	fieldDur("probe_timeout", func(c *config.Config) time.Duration { return c.ProbeTimeout }, func(c *config.Config) *time.Duration { return &c.ProbeTimeout }),
	fieldStr("egress_device", func(c *config.Config) string { return c.EgressDevice }, func(c *config.Config) *string { return &c.EgressDevice }, validateNonEmpty),
	fieldInt("stable_rounds", func(c *config.Config) int { return c.StableRounds }, func(c *config.Config) *int { return &c.StableRounds }, 1),
	fieldStr("final_ip", func(c *config.Config) string { return c.FinalIP }, func(c *config.Config) *string { return &c.FinalIP }, validateIPv4),
}

// ---------- 编辑器状态 ----------

type editMode int

const (
	modeNav editMode = iota
	modeInput
	modeForm
)

// inputTask 是行内编辑任务（编辑 target / 新增 target 的字段序列）。
type inputTask struct {
	label string
	cur   string
	apply func(string) error
	skip  func() bool // 为 true 时跳过此任务（如探测方式不匹配的端口/URL）
}

// targetFormField 是上游实例表单中的一个字段。和全局配置字段一样，
// 字段只在按回车时进入输入状态，字段之间可以用上下键切换。
type targetFormField struct {
	label string
	get   func(*config.Target) string
	apply func(*config.Target, string) error
}

type cfgEditor struct {
	cfg *config.Config
	c   *control.Client

	sel  int // 当前选中行：0..len(globalFields)-1 为全局字段，其后为上游
	mode editMode

	// 行内输入状态
	inputPrompt string
	inputCur    string // 空输入时的保留值
	inputBuf    []byte
	inputApply  func(string) error
	inputReturn editMode // 输入完成后返回的模式（普通导航或上游表单）

	// 字段序列状态（target 编辑/新增）
	taskQueue []inputTask
	taskIdx   int
	confirm   *func() // 待确认动作（y 执行）

	// 上游实例表单状态。表单使用副本编辑，按 s 后才写回配置；按 q/ESC
	// 则丢弃本次修改。
	formActive    bool
	formTarget    config.Target
	formIdx       int
	formTargetIdx int // 编辑时为配置中的下标，新增时为 -1
	formOrigName  string

	msg  string
	quit bool
}

func (e *cfgEditor) totalLines() int { return len(globalFields) + len(e.cfg.Targets) }

func (e *cfgEditor) run() error {
	if !term.IsTerminal(int(os.Stdout.Fd())) || !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("配置编辑器需要终端（TTY）运行")
	}
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("设置终端原始模式失败: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	e.render()
	for !e.quit {
		ev, err := e.readKey()
		if err != nil {
			return err
		}
		e.handle(ev)
		e.render()
	}
	return nil
}

// ---------- 按键读取 ----------

// readKey 从 stdin 读取一个按键事件（含方向键转义序列），供各 TUI 复用。
func readKey() (keyEvent, error) {
	return readKeyStdin()
}

func readKeyStdin() (keyEvent, error) {
	buf := make([]byte, 1)
	n, err := os.Stdin.Read(buf)
	if err != nil || n == 0 {
		return keyEvent{act: actNone}, err
	}
	b := buf[0]
	if b == 0x1b {
		// 转义序列：ESC [ A/B/C/D（方向键），单独 ESC 视为取消。
		// 用 poll 等后续字节（50ms），不用 SetReadDeadline——对终端不生效。
		pfd := make([]unix.PollFd, 1)
		pfd[0] = unix.PollFd{Fd: int32(os.Stdin.Fd()), Events: unix.POLLIN}
		if n, _ := unix.Poll(pfd, 50); n > 0 {
			rest := make([]byte, 2)
			n2, _ := os.Stdin.Read(rest)
			if n2 == 2 && rest[0] == '[' {
				switch rest[1] {
				case 'A':
					return keyEvent{act: actUp}, nil
				case 'B':
					return keyEvent{act: actDown}, nil
				}
			}
		}
		return keyEvent{act: actEsc}, nil
	}
	switch b {
	case '\r':
		return keyEvent{act: actEnter}, nil
	case 0x03:
		return keyEvent{act: actNone}, nil // Ctrl+C 不再支持退出，忽略
	case 0x7f, 0x08:
		return keyEvent{act: actBackspace}, nil
	default:
		return keyEvent{act: actChar, ch: b}, nil
	}
}

func (e *cfgEditor) readKey() (keyEvent, error) { return readKey() }

// ---------- 事件处理 ----------

func (e *cfgEditor) handle(ev keyEvent) {
	if e.mode == modeInput {
		e.handleInput(ev)
		return
	}
	if e.mode == modeForm {
		e.handleForm(ev)
		return
	}
	switch ev.act {
	case actUp:
		if e.sel > 0 {
			e.sel--
		}
	case actDown:
		if e.sel < e.totalLines()-1 {
			e.sel++
		}
	case actEnter:
		e.editSel()
	case actChar:
		switch ev.ch {
		case 'a':
			e.addTarget()
		case 'd':
			e.deleteTarget()
		case 's':
			e.save()
		case 'q':
			e.quit = true
		}
	case actEsc:
		e.quit = true
	}
}

func (e *cfgEditor) handleForm(ev keyEvent) {
	fields := targetFormFields(e)
	if len(fields) == 0 {
		return
	}
	switch ev.act {
	case actUp:
		if e.formIdx > 0 {
			e.formIdx--
		}
	case actDown:
		if e.formIdx < len(fields)-1 {
			e.formIdx++
		}
	case actEnter:
		f := fields[e.formIdx]
		e.startFormInput("编辑 "+f.label, f.get(&e.formTarget), func(s string) error {
			if err := f.apply(&e.formTarget, s); err != nil {
				return err
			}
			// 探测方式变化会改变表单中的端口/URL字段。
			if e.formIdx >= len(targetFormFields(e))-1 {
				e.formIdx = len(targetFormFields(e)) - 1
			}
			return nil
		})
	case actChar:
		switch ev.ch {
		case 's':
			e.commitTargetForm()
		case 'q':
			e.cancelTargetForm()
		}
	case actEsc:
		e.cancelTargetForm()
	}
}

func (e *cfgEditor) handleInput(ev keyEvent) {
	switch ev.act {
	case actChar:
		e.inputBuf = append(e.inputBuf, ev.ch)
	case actBackspace:
		if len(e.inputBuf) > 0 {
			e.inputBuf = e.inputBuf[:len(e.inputBuf)-1]
		}
	case actEnter:
		val := string(e.inputBuf)
		if val == "" {
			val = e.inputCur // 空输入 = 保留当前值
		}
		if e.confirm != nil {
			fn := *e.confirm
			e.confirm = nil
			e.mode = modeNav
			if strings.EqualFold(strings.TrimSpace(val), "y") {
				fn()
			} else {
				e.msg = "已取消"
			}
			return
		}
		if err := e.inputApply(val); err != nil {
			e.msg = "无效输入: " + err.Error()
			e.inputBuf = nil // 清空重输
			return
		}
		e.mode = e.inputReturn
		if e.inputReturn == modeNav {
			e.nextTask()
		}
	case actEsc:
		e.confirm = nil
		e.mode = e.inputReturn
		e.msg = "已取消"
	}
}

// editSel 编辑当前选中行（全局字段或上游）。
func (e *cfgEditor) editSel() {
	if e.sel < len(globalFields) {
		f := globalFields[e.sel]
		e.startInput("编辑 "+f.label, f.get(e.cfg), func(s string) error {
			return f.apply(e.cfg, s)
		})
		return
	}
	idx := e.sel - len(globalFields)
	if idx >= len(e.cfg.Targets) {
		return
	}
	e.editTarget(idx)
}

// startInput 启动一次行内输入。
func (e *cfgEditor) startInput(label, cur string, apply func(string) error) {
	e.mode = modeInput
	e.inputPrompt = label + " [" + cur + "]: "
	e.inputCur = cur
	e.inputBuf = nil
	e.inputApply = apply
	e.inputReturn = modeNav
	e.confirm = nil
}

func (e *cfgEditor) startFormInput(label, cur string, apply func(string) error) {
	e.mode = modeInput
	e.inputPrompt = label + " [" + cur + "]: "
	e.inputCur = cur
	e.inputBuf = nil
	e.inputApply = apply
	e.inputReturn = modeForm
	e.confirm = nil
}

// nextTask 推进字段序列；队列清空则回到导航。
func (e *cfgEditor) nextTask() {
	for e.taskIdx < len(e.taskQueue) {
		t := e.taskQueue[e.taskIdx]
		e.taskIdx++
		if t.skip != nil && t.skip() {
			continue // 按条件跳过
		}
		e.startInput(t.label, t.cur, t.apply)
		return
	}
	e.taskQueue = nil
	e.msg = "修改完成（按 s 保存并热加载生效）"
}

// ---------- 上游编辑 / 新增 / 删除 ----------

// targetTasks 为指定 target 生成字段编辑任务序列。
func targetTasks(t *config.Target, editing bool) []inputTask {
	nameCur := ""
	ipCur := ""
	wCur := ""
	pCur := ""
	if editing {
		nameCur = t.Name
		ipCur = t.IP
		wCur = strconv.Itoa(t.Weight)
		pCur = string(t.Probe)
	}
	tasks := []inputTask{
		{label: "名字", cur: nameCur, apply: func(s string) error {
			s = strings.TrimSpace(s)
			if s == "" {
				return fmt.Errorf("名字不能为空")
			}
			t.Name = s
			return nil
		}},
		{label: "IP 地址", cur: ipCur, apply: func(s string) error {
			if err := validateIPv4(strings.TrimSpace(s)); err != nil {
				return err
			}
			t.IP = strings.TrimSpace(s)
			return nil
		}},
		{label: "权重", cur: wCur, apply: func(s string) error {
			n, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil {
				return fmt.Errorf("%q 不是整数", s)
			}
			if n < 0 {
				return fmt.Errorf("权重不能为负")
			}
			t.Weight = n
			return nil
		}},
		{label: "探测方式 (ping/tcp/udp/http)", cur: pCur, apply: func(s string) error {
			m := config.ProbeMethod(strings.ToLower(strings.TrimSpace(s)))
			switch m {
			case config.ProbePing, config.ProbeTCP, config.ProbeUDP, config.ProbeHTTP:
				t.Probe = m
				// 清理与探测方式不适配的字段
				switch m {
				case config.ProbePing:
					t.Port, t.URL = 0, ""
				case config.ProbeTCP, config.ProbeUDP:
					t.URL = ""
				case config.ProbeHTTP:
					t.Port = 0
				}
				return nil
			}
			return fmt.Errorf("未知探测方式 %q（支持 ping/tcp/udp/http）", s)
		}},
	}
	// 端口/URL 任务总是加入队列，执行时按最终探测方式决定是否跳过
	// （probe 任务在前，选择完成后才能确定；不能静态追加）。
	tasks = append(tasks,
		inputTask{
			label: "端口 (1-65535)",
			cur: func() string {
				if editing && t.Port != 0 {
					return strconv.Itoa(t.Port)
				}
				return ""
			}(),
			skip: func() bool { return t.Probe != config.ProbeTCP && t.Probe != config.ProbeUDP },
			apply: func(s string) error {
				n, err := strconv.Atoi(strings.TrimSpace(s))
				if err != nil || n < 1 || n > 65535 {
					return fmt.Errorf("端口必须为 1-65535")
				}
				t.Port = n
				return nil
			},
		},
		inputTask{
			label: "完整 URL（如 http://1.2.3.4/healthz）",
			cur: func() string {
				if editing {
					return t.URL
				}
				return ""
			}(),
			skip: func() bool { return t.Probe != config.ProbeHTTP },
			apply: func(s string) error {
				s = strings.TrimSpace(s)
				if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
					return fmt.Errorf("%q 必须以 http:// 或 https:// 开头", s)
				}
				t.URL = s
				return nil
			},
		},
	)
	return tasks
}

// targetFormFields 返回当前上游实例的表单字段。端口和 URL 根据探测方式动态显示，
// 因此修改 probe 后，表单会立即切换到对应的专用字段。
func targetFormFields(e *cfgEditor) []targetFormField {
	fields := []targetFormField{
		{
			label: "名字",
			get:   func(t *config.Target) string { return t.Name },
			apply: func(t *config.Target, s string) error {
				s = strings.TrimSpace(s)
				if s == "" {
					return fmt.Errorf("名字不能为空")
				}
				for i, x := range e.cfg.Targets {
					if i != e.formTargetIdx && x.Name == s {
						return fmt.Errorf("名字 %q 已存在", s)
					}
				}
				t.Name = s
				return nil
			},
		},
		{
			label: "IP 地址",
			get:   func(t *config.Target) string { return t.IP },
			apply: func(t *config.Target, s string) error {
				s = strings.TrimSpace(s)
				if err := validateIPv4(s); err != nil {
					return err
				}
				t.IP = s
				return nil
			},
		},
		{
			label: "权重",
			get:   func(t *config.Target) string { return strconv.Itoa(t.Weight) },
			apply: func(t *config.Target, s string) error {
				n, err := strconv.Atoi(strings.TrimSpace(s))
				if err != nil {
					return fmt.Errorf("%q 不是整数", s)
				}
				if n < 0 {
					return fmt.Errorf("权重不能为负")
				}
				t.Weight = n
				return nil
			},
		},
		{
			label: "探测方式 (ping/tcp/udp/http)",
			get:   func(t *config.Target) string { return string(t.Probe) },
			apply: func(t *config.Target, s string) error {
				m := config.ProbeMethod(strings.ToLower(strings.TrimSpace(s)))
				switch m {
				case config.ProbePing, config.ProbeTCP, config.ProbeUDP, config.ProbeHTTP:
					t.Probe = m
					switch m {
					case config.ProbePing:
						t.Port, t.URL = 0, ""
					case config.ProbeTCP, config.ProbeUDP:
						t.URL = ""
					case config.ProbeHTTP:
						t.Port = 0
					}
					return nil
				default:
					return fmt.Errorf("未知探测方式 %q（支持 ping/tcp/udp/http）", s)
				}
			},
		},
	}
	if e.formTarget.Probe == config.ProbeTCP || e.formTarget.Probe == config.ProbeUDP {
		fields = append(fields, targetFormField{
			label: "端口 (1-65535)",
			get: func(t *config.Target) string {
				if t.Port == 0 {
					return ""
				}
				return strconv.Itoa(t.Port)
			},
			apply: func(t *config.Target, s string) error {
				n, err := strconv.Atoi(strings.TrimSpace(s))
				if err != nil || n < 1 || n > 65535 {
					return fmt.Errorf("端口必须为 1-65535")
				}
				t.Port = n
				return nil
			},
		})
	}
	if e.formTarget.Probe == config.ProbeHTTP {
		fields = append(fields, targetFormField{
			label: "完整 URL（如 http://1.2.3.4/healthz）",
			get:   func(t *config.Target) string { return t.URL },
			apply: func(t *config.Target, s string) error {
				s = strings.TrimSpace(s)
				if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
					return fmt.Errorf("%q 必须以 http:// 或 https:// 开头", s)
				}
				t.URL = s
				return nil
			},
		})
	}
	return fields
}

func (e *cfgEditor) editTarget(idx int) {
	if idx < 0 || idx >= len(e.cfg.Targets) {
		return
	}
	e.formTarget = e.cfg.Targets[idx]
	e.formTargetIdx = idx
	e.formOrigName = e.formTarget.Name
	e.formIdx = 0
	e.formActive = true
	e.mode = modeForm
	e.msg = ""
}

func (e *cfgEditor) addTarget() {
	e.formTarget = config.Target{Weight: 100, Probe: config.ProbePing}
	e.formTargetIdx = -1
	e.formOrigName = ""
	e.formIdx = 0
	e.formActive = true
	e.mode = modeForm
	e.msg = ""
}

func (e *cfgEditor) commitTargetForm() {
	if !e.formActive {
		return
	}
	if e.formTarget.Name == "" {
		e.msg = "保存失败: 名字不能为空"
		return
	}
	for i, t := range e.cfg.Targets {
		if i != e.formTargetIdx && t.Name == e.formTarget.Name {
			e.msg = "保存失败: 名字 " + fmt.Sprintf("%q", t.Name) + " 已存在"
			return
		}
	}
	trial := *e.cfg
	trial.Targets = append([]config.Target(nil), e.cfg.Targets...)
	if e.formTargetIdx < 0 {
		trial.Targets = append(trial.Targets, e.formTarget)
	} else {
		trial.Targets[e.formTargetIdx] = e.formTarget
	}
	if err := trial.Validate(); err != nil {
		e.msg = "保存失败: " + err.Error()
		return
	}
	if e.formTargetIdx < 0 {
		e.cfg.Targets = append(e.cfg.Targets, e.formTarget)
		e.sel = len(globalFields) + len(e.cfg.Targets) - 1
		e.msg = "已新增上游 " + e.formTarget.Name + "（按 s 保存生效）"
	} else {
		e.cfg.Targets[e.formTargetIdx] = e.formTarget
		e.sel = len(globalFields) + e.formTargetIdx
		e.msg = "已修改上游 " + e.formTarget.Name + "（按 s 保存生效）"
	}
	e.formActive = false
	e.mode = modeNav
}

func (e *cfgEditor) cancelTargetForm() {
	e.formActive = false
	e.mode = modeNav
	e.msg = "已取消"
}

func (e *cfgEditor) deleteTarget() {
	idx := e.sel - len(globalFields)
	if idx < 0 || idx >= len(e.cfg.Targets) {
		e.msg = "请先选中一个上游再按 d 删除"
		return
	}
	name := e.cfg.Targets[idx].Name
	doDelete := func() {
		e.cfg.Targets = append(e.cfg.Targets[:idx], e.cfg.Targets[idx+1:]...)
		if e.sel >= e.totalLines() {
			e.sel = e.totalLines() - 1
		}
		e.msg = "已删除上游 " + name + "（按 s 保存生效）"
	}
	e.confirm = &doDelete
	e.mode = modeInput
	e.inputPrompt = "确认删除上游 " + name + "? (y/N): "
	e.inputCur = "n"
	e.inputBuf = nil
	e.inputApply = func(s string) error { return nil }
	e.inputReturn = modeNav
}

// ---------- 保存 ----------

func (e *cfgEditor) save() {
	if err := e.cfg.Validate(); err != nil {
		e.msg = "保存失败: " + err.Error()
		return
	}
	data, err := yaml.Marshal(e.cfg)
	if err != nil {
		e.msg = "序列化失败: " + err.Error()
		return
	}
	if err := e.c.PostConfig(data); err != nil {
		e.msg = "保存失败: " + err.Error()
		return
	}
	e.msg = "已保存并热加载生效"
}

// ---------- 渲染 ----------

func (e *cfgEditor) render() {
	var sb strings.Builder
	sb.WriteString("\x1b[H")
	if e.mode == modeForm || (e.mode == modeInput && e.inputReturn == modeForm) {
		e.renderTargetForm(&sb)
		sb.WriteString("\x1b[K\x1b[J")
		fmt.Print(sb.String())
		return
	}
	sb.WriteString("nexthop 配置编辑器  ↑/↓ 选择  回车 编辑  a 新增  d 删除  s 保存  q/ESC 退出")
	sb.WriteString("\x1b[K\r\n")
	sb.WriteString("── 全局配置 ──────────────────────────────\x1b[K\r\n")
	for i, f := range globalFields {
		sb.WriteString(e.renderLine(i, fmt.Sprintf("  %s %s", padRight(f.label, 16), f.get(e.cfg))))
		sb.WriteString("\x1b[K\r\n")
	}
	sb.WriteString("── 上游 ──────────────────────────────────\x1b[K\r\n")
	for i := range e.cfg.Targets {
		t := &e.cfg.Targets[i]
		extra := ""
		switch t.Probe {
		case config.ProbeTCP, config.ProbeUDP:
			extra = fmt.Sprintf(":%d", t.Port)
		case config.ProbeHTTP:
			extra = " " + t.URL
		}
		line := fmt.Sprintf("  [%d] %s %s weight=%-5d probe %s%s",
			i+1, padRight(t.Name, 14), padRight(t.IP, 15), t.Weight, t.Probe, extra)
		sb.WriteString(e.renderLine(len(globalFields)+i, line))
		sb.WriteString("\x1b[K\r\n")
	}
	// 底部：输入行或消息
	if e.mode == modeInput {
		sb.WriteString("\x1b[K\r\n" + e.inputPrompt + string(e.inputBuf))
	} else {
		sb.WriteString("\x1b[K\r\n" + e.msg)
	}
	sb.WriteString("\x1b[K\x1b[J")
	fmt.Print(sb.String())
}

func (e *cfgEditor) renderTargetForm(sb *strings.Builder) {
	title := "编辑上游"
	if e.formTargetIdx < 0 {
		title = "新增上游"
	}
	sb.WriteString("nexthop 配置编辑器  " + title + "  ↑/↓ 选择  回车 编辑  s 确认  q/ESC 取消")
	sb.WriteString("\x1b[K\r\n")
	sb.WriteString("── 上游实例 ──────────────────────────────\x1b[K\r\n")
	for i, f := range targetFormFields(e) {
		line := fmt.Sprintf("  %-38s %s", f.label, f.get(&e.formTarget))
		if e.mode == modeForm && i == e.formIdx {
			line = "\x1b[7m" + line + "\x1b[0m"
		}
		sb.WriteString(line)
		sb.WriteString("\x1b[K\r\n")
	}
	if e.mode == modeInput {
		sb.WriteString("\x1b[K\r\n" + e.inputPrompt + string(e.inputBuf))
	} else {
		sb.WriteString("\x1b[K\r\n" + e.msg)
	}
}

func (e *cfgEditor) renderLine(line int, content string) string {
	if line == e.sel {
		return "\x1b[7m" + content + "\x1b[0m"
	}
	return content
}
