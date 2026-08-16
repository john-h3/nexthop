package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"nexthop/internal/config"
	"nexthop/internal/control"
	"nexthop/internal/state"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// newControlClient 解析通用控制选项并构造 unix socket 客户端。
func newControlClient(args []string, name string) (*control.Client, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	sockPath := fs.String("s", defaultSockPath, "control unix socket 路径")
	fs.StringVar(sockPath, "socket", defaultSockPath, "control unix socket 路径")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return control.NewClient(*sockPath), nil
}

// cmdStatus 查看实时状态；-w 进入刷新模式（每 interval 刷新，q 或 ESC 退出）。
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	sockPath := fs.String("s", defaultSockPath, "control unix socket 路径")
	fs.StringVar(sockPath, "socket", defaultSockPath, "control unix socket 路径")
	once := fs.Bool("o", false, "单次输出（默认在终端下自动进入刷新模式）")
	interval := fs.Duration("i", 100*time.Millisecond, "刷新间隔")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c := control.NewClient(*sockPath)
	// 终端（stdout 与 stdin 均为 TTY）默认进入刷新模式；
	// 管道/脚本等非终端环境自动退化为单次输出。
	if !*once && term.IsTerminal(int(os.Stdout.Fd())) && term.IsTerminal(int(os.Stdin.Fd())) {
		return watchStatus(c, *interval)
	}
	return printStatus(c)
}

// statusText 生成当前状态的文本（单次输出与刷新模式共用的唯一格式来源）。
func statusText(c *control.Client) (string, error) {
	st, err := c.GetStatus()
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	active := st.ActiveNexthop
	if active == "" {
		active = "（尚未设置）"
	}
	fmt.Fprintf(&sb, "当前生效上游: %s\n", active)
	if st.FinalActive {
		fmt.Fprintf(&sb, "  ※ final 兜底激活中：所有上游不可用\n")
	}
	fmt.Fprintf(&sb, "\n目标:\n")
	for _, t := range sortedByWeight(st.Targets) {
		status := "down"
		if t.Up {
			status = "up  "
		}
		detail := ""
		if t.LastRTT > 0 {
			detail = fmt.Sprintf("rtt %s", t.LastRTT.Round(time.Microsecond))
		}
		if t.LastErr != "" {
			detail = "err: " + t.LastErr
		}
		// 用显示宽度对齐（CJK/全角字符占 2 列），避免中文名导致列错位。
		fmt.Fprintf(&sb, "  %s %s %s %s  %-5d %s\n",
			padRight(t.Name, 16), padRight(t.IP, 15), padRight(t.Probe, 5), padRight(status, 4), t.Weight, detail)
	}
	return sb.String(), nil
}

// watchStatusText 生成实时状态与配置的合并视图（主菜单“实时状态 + 配置列表”用）：
// 头部为当前生效上游与全局配置概要，随后按名称关联状态与配置，一张表展示所有上游。
func watchStatusText(c *control.Client) (string, error) {
	st, err := c.GetStatus()
	if err != nil {
		return "", err
	}
	data, err := c.GetConfig()
	if err != nil {
		return "", err
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return "", err
	}

	byName := make(map[string]state.TargetStatus, len(st.Targets))
	for _, t := range st.Targets {
		byName[t.Name] = t
	}

	var sb strings.Builder
	active := st.ActiveNexthop
	if active == "" {
		active = "（尚未设置）"
	}
	fmt.Fprintf(&sb, "当前生效上游: %s\n", active)
	if st.FinalActive {
		fmt.Fprintf(&sb, "  ※ final 兜底激活中：所有上游不可用\n")
	}
	fmt.Fprintf(&sb, "探测间隔: %s   超时: %s   出口网卡: %s   防抖轮数: %d   final 兜底: %s\n\n",
		cfg.ProbeInterval, cfg.ProbeTimeout, cfg.EgressDevice, cfg.StableRounds, cfg.FinalIP)
	fmt.Fprintf(&sb, "上游:\n")
	for _, t := range cfg.SortedTargets() {
		addr := t.IP
		switch t.Probe {
		case config.ProbeTCP, config.ProbeUDP:
			addr = fmt.Sprintf("%s:%d", t.IP, t.Port)
		case config.ProbeHTTP:
			addr = t.URL
		}
		status := "?"
		detail := ""
		if s, ok := byName[t.Name]; ok {
			if s.Up {
				status = "up"
			} else {
				status = "down"
			}
			if s.LastRTT > 0 {
				detail = "rtt " + s.LastRTT.Round(time.Microsecond).String()
			}
			if s.LastErr != "" {
				detail = "err: " + s.LastErr
			}
		}
		fmt.Fprintf(&sb, "  %s %s %s %s  %-5d %s\n",
			padRight(t.Name, 16), padRight(addr, 22), padRight(string(t.Probe), 6), padRight(status, 5), t.Weight, detail)
	}
	return sb.String(), nil
}

// printStatus 单次输出当前状态。
func printStatus(c *control.Client) error {
	text, err := statusText(c)
	if err != nil {
		return err
	}
	fmt.Print(text)
	return nil
}

// watchStatus 每 interval 刷新一次状态（覆盖式重绘，无清屏闪动），按 q 或 ESC 退出。
// 需要终端（TTY）：raw 模式下按 q/ESC 即时响应。
func watchStatus(c *control.Client, interval time.Duration) error {
	if !term.IsTerminal(int(os.Stdout.Fd())) || !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("刷新模式需要终端（TTY）运行，请直接执行 status 或改用 -w 并在终端中运行")
	}
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("设置终端原始模式失败: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	fmt.Print("\x1b[?25l")          // 隐藏光标
	defer fmt.Print("\x1b[?25h\n")  // 退出时恢复光标并换行

	for {
		text, err := watchStatusText(c)
		if err != nil {
			return err
		}
		renderStatus(text, interval)
		// poll 等待按键：stdin 可读才 readKey（不会阻塞），超时（=刷新间隔）则刷新下一帧。
		// 不能用 os.Stdin.SetReadDeadline——实测对终端不生效，Read 会一直阻塞、
		// 自动刷新永不触发；也不用后台 goroutine 读键——退出后 goroutine 残留阻塞在
		// os.Stdin.Read，会抢占 stdin 导致外层 TUI（主菜单）按键被吞。
		pfd := make([]unix.PollFd, 1)
		pfd[0] = unix.PollFd{Fd: int32(os.Stdin.Fd()), Events: unix.POLLIN}
		timeoutMs := int(interval.Milliseconds())
		if timeoutMs < 1 {
			timeoutMs = 1
		}
		n, err := unix.Poll(pfd, timeoutMs)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return err
		}
		if n == 0 {
			continue // 超时：刷新下一帧
		}
		ev, err := readKey()
		if err != nil {
			return err
		}
		if (ev.act == actChar && ev.ch == 'q') || ev.act == actEsc {
			return nil
		}
	}
}

// renderStatus 覆盖式重绘状态帧：光标回顶部，逐行覆盖并清除行尾残留，
// 最后清除多余行（不做整屏清空，避免刷新闪动）。
func renderStatus(text string, interval time.Duration) {
	fmt.Print("\x1b[H")
	body := strings.TrimSuffix(text, "\n")
	if body != "" {
		for _, line := range strings.Split(body, "\n") {
			fmt.Print(line)
			fmt.Print("\x1b[K\r\n")
		}
	}
	fmt.Print("（每 ", interval.String(), " 刷新，按 q 或 ESC 退出）")
	fmt.Print("\x1b[K\r\n")
	fmt.Print("\x1b[J")
}

// configListText 生成配置列表文本（cmdList 与主菜单复用）。
func configListText(c *control.Client) (string, error) {
	data, err := c.GetConfig()
	if err != nil {
		return "", err
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "探测间隔: %s   超时: %s   出口网卡: %s   防抖轮数: %d\n",
		cfg.ProbeInterval, cfg.ProbeTimeout, cfg.EgressDevice, cfg.StableRounds)
	fmt.Fprintf(&sb, "final 兜底: %s\n\n", cfg.FinalIP)
	fmt.Fprintf(&sb, "上游:\n")
	for _, t := range cfg.SortedTargets() {
		extra := ""
		switch t.Probe {
		case config.ProbeTCP, config.ProbeUDP:
			extra = fmt.Sprintf(":%d", t.Port)
		case config.ProbeHTTP:
			extra = " " + t.URL
		}
		fmt.Fprintf(&sb, "  %s %s%s weight %-5d probe %s%s\n",
			padRight(t.Name, 16), padRight(t.IP, 15), padRight("", 8), t.Weight, t.Probe, extra)
	}
	return sb.String(), nil
}

func cmdList(args []string) error {
	c, err := newControlClient(args, "list")
	if err != nil {
		return err
	}
	text, err := configListText(c)
	if err != nil {
		return err
	}
	fmt.Print(text)
	return nil
}

func cmdReload(args []string) error {
	c, err := newControlClient(args, "reload")
	if err != nil {
		return err
	}
	if err := c.Reload(); err != nil {
		return err
	}
	fmt.Println("配置已重新加载")
	return nil
}

func sortedByWeight(ts []state.TargetStatus) []state.TargetStatus {
	out := make([]state.TargetStatus, len(ts))
	copy(out, ts)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].Weight < out[j].Weight; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// cmdAdd 交互式引导用户输入新上游，经 daemon 校验、持久化并热加载。
func cmdAdd(args []string) error {
	c, err := newControlClient(args, "add")
	if err != nil {
		return err
	}
	data, err := c.GetConfig()
	if err != nil {
		return err
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return fmt.Errorf("daemon 返回的配置无法解析: %w", err)
	}

	reader := bufio.NewReader(os.Stdin)
	fmt.Println("交互式增加上游（输入无效则重试）")
	fmt.Println()

	t := config.Target{}

	t.Name = prompt(reader, "名字", "", func(s string) error {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("名字不能为空")
		}
		for _, existing := range cfg.Targets {
			if existing.Name == s {
				return fmt.Errorf("名字 %q 已存在", s)
			}
		}
		return nil
	})

	t.IP = prompt(reader, "IP 地址", "", func(s string) error {
		ip := net.ParseIP(strings.TrimSpace(s))
		if ip == nil || ip.To4() == nil {
			return fmt.Errorf("%q 不是合法的 IPv4 地址", s)
		}
		return nil
	})

	t.Weight = promptInt(reader, "权重", 100, func(n int) error {
		if n < 0 {
			return fmt.Errorf("权重不能为负")
		}
		return nil
	})

	t.Probe = config.ProbeMethod(prompt(reader, "探测方式 (ping/tcp/udp/http)", "ping", func(s string) error {
		switch config.ProbeMethod(strings.ToLower(strings.TrimSpace(s))) {
		case config.ProbePing, config.ProbeTCP, config.ProbeUDP, config.ProbeHTTP:
			return nil
		}
		return fmt.Errorf("未知探测方式 %q（支持 ping/tcp/udp/http）", s)
	}))

	switch t.Probe {
	case config.ProbeTCP, config.ProbeUDP:
		t.Port = promptInt(reader, "端口 (1-65535)", 0, func(n int) error {
			if n < 1 || n > 65535 {
				return fmt.Errorf("端口 %d 不合法（1-65535）", n)
			}
			return nil
		})
	case config.ProbeHTTP:
		t.URL = prompt(reader, "完整 URL（如 http://1.2.3.4:8443/healthz）", "", func(s string) error {
			s = strings.TrimSpace(s)
			if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
				return fmt.Errorf("%q 必须以 http:// 或 https:// 开头", s)
			}
			return nil
		})
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("新配置校验失败（未保存）: %w", err)
	}

	fmt.Println()
	fmt.Printf("即将添加上游: %s (%s) weight=%d probe=%s\n", t.Name, t.IP, t.Weight, t.Probe)
	ok := prompt(reader, "确认添加 (y/N)", "n", func(s string) error { return nil })
	if !strings.EqualFold(strings.TrimSpace(ok), "y") {
		fmt.Println("已取消")
		return nil
	}

	cfg.Targets = append(cfg.Targets, t)
	newData, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := c.PostConfig(newData); err != nil {
		return err
	}
	fmt.Printf("已添加上游 %q 并热加载生效\n", t.Name)
	return nil
}

// prompt 带校验地读取一行输入；默认值在输入为空时生效。
func prompt(r *bufio.Reader, label, def string, validate func(string) error) string {
	for {
		hint := ""
		if def != "" {
			hint = fmt.Sprintf(" [%s]", def)
		}
		fmt.Printf("  %s%s: ", label, hint)
		line, err := r.ReadString('\n')
		if err != nil {
			fmt.Println()
			os.Exit(1)
		}
		s := strings.TrimSpace(line)
		if s == "" {
			s = def
		}
		if err := validate(s); err != nil {
			fmt.Printf("    ✗ %v，请重试\n", err)
			continue
		}
		return s
	}
}

func promptInt(r *bufio.Reader, label string, def int, validate func(int) error) int {
	for {
		hint := ""
		if def != 0 {
			hint = fmt.Sprintf(" [%d]", def)
		}
		fmt.Printf("  %s%s: ", label, hint)
		line, err := r.ReadString('\n')
		if err != nil {
			fmt.Println()
			os.Exit(1)
		}
		s := strings.TrimSpace(line)
		if s == "" && def != 0 {
			s = strconv.Itoa(def)
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			fmt.Printf("    ✗ %q 不是整数，请重试\n", s)
			continue
		}
		if err := validate(n); err != nil {
			fmt.Printf("    ✗ %v，请重试\n", err)
			continue
		}
		return n
	}
}
