package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"nexthop/internal/control"

	"golang.org/x/term"
)

// cmdTUI 进入主菜单总控台：所有功能（状态/配置/新增/列表/热加载/安装/卸载）
// 都在菜单中执行，子功能退出后返回主菜单。用法：nexthop [-s <socket>]
func cmdTUI(args []string) error {
	fs := flag.NewFlagSet("nexthop", flag.ContinueOnError)
	sockPath := fs.String("s", defaultSockPath, "control unix socket 路径")
	fs.StringVar(sockPath, "socket", defaultSockPath, "control unix socket 路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !term.IsTerminal(int(os.Stdout.Fd())) || !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("交互界面需要终端（TTY）运行；脚本环境请使用子命令（如 nexthop status -o）")
	}
	c := control.NewClient(*sockPath)
	m := &mainMenu{c: c}
	return m.run()
}

// menuItem 是主菜单的一个功能项。
type menuItem struct {
	label string
	exec  func(*mainMenu)
}

var menuItems = []menuItem{
	{"实时状态 + 配置列表（自动刷新，q/ESC 退出）", func(m *mainMenu) { m.runSub(func() error { return watchStatus(m.c, 100*time.Millisecond) }) }},
	{"编辑配置（全局 + 上游）", func(m *mainMenu) { m.runSub(func() error { e, err := newCfgEditor(m.c); if err != nil { return err }; return e.run() }) }},
	{"新增上游", func(m *mainMenu) { m.runSub(func() error { w, err := newAddWizard(m.c); if err != nil { return err }; return w.run() }) }},
	{"热加载配置", func(m *mainMenu) { m.doAndWait("热加载", func() error { return m.c.Reload() }) }},
	{"启动服务", func(m *mainMenu) { m.serviceAction("启动", "start") }},
	{"停止服务", func(m *mainMenu) { m.serviceAction("停止", "stop") }},
	{"安装为系统服务", func(m *mainMenu) { m.doAndWait("安装", func() error { return cmdInstall(nil) }) }},
	{"卸载并清理", func(m *mainMenu) { m.doAndWait("卸载", func() error { return cmdUninstall(nil) }) }},
}

type mainMenu struct {
	c        *control.Client
	sel      int
	msg      string
	oldState *term.State // 主菜单 MakeRaw 前的终端状态（用于嵌套子 TUI）
}

func (m *mainMenu) run() error {
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("设置终端原始模式失败: %w", err)
	}
	m.oldState = oldState
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	m.render()
	for {
		ev, err := readKey()
		if err != nil {
			return err
		}
		switch ev.act {
		case actUp:
			if m.sel > 0 {
				m.sel--
				m.render()
			}
		case actDown:
			if m.sel < len(menuItems)-1 {
				m.sel++
				m.render()
			}
		case actEnter:
			menuItems[m.sel].exec(m)
			m.render()
		case actChar:
			switch ev.ch {
			case 'q':
				return nil
			case '1', '2', '3', '4', '5', '6', '7', '8', '9':
				idx := int(ev.ch - '1')
				if idx < len(menuItems) {
					m.sel = idx
					menuItems[idx].exec(m)
					m.render()
				}
			}
		case actEsc:
			return nil
		}
	}
}

// enterCooked 退出主菜单的 raw 模式，让子界面用普通 Println 输出。
// raw 模式下 \n 不会自动回车（终端的 ONLCR 被关闭），多行文本换行会错位，
// 出现“换行后开头不在行首”的乱象。
func (m *mainMenu) enterCooked() {
	_ = term.Restore(int(os.Stdin.Fd()), m.oldState)
}

// leaveCooked 恢复主菜单的 raw 模式以便读取按键。
func (m *mainMenu) leaveCooked() bool {
	if _, err := term.MakeRaw(int(os.Stdin.Fd())); err != nil {
		m.msg = "恢复终端失败: " + err.Error()
		return false
	}
	return true
}

// serviceAction 启动/停止 OpenRC 服务（依赖 nexthop install 已安装）。
func (m *mainMenu) serviceAction(label, action string) {
	if _, err := os.Stat("/etc/init.d/nexthop"); err != nil {
		m.msg = "服务未安装，请先在菜单选择“安装为系统服务”"
		return
	}
	m.enterCooked()
	fmt.Print("\x1b[2J\x1b[H")
	fmt.Println("── " + label + "服务 ──")
	if out, err := exec.Command("rc-service", "nexthop", action).CombinedOutput(); err != nil {
		fmt.Println("失败:", err)
		fmt.Print(strings.TrimRight(string(out), "\n"))
		fmt.Println()
	} else {
		fmt.Println("完成")
		fmt.Print(strings.TrimRight(string(out), "\n"))
		fmt.Println()
	}
	fmt.Println()
	fmt.Println("（按任意键返回主菜单）")
	if !m.leaveCooked() {
		return
	}
	_, _ = readKey()
}

// runSub 运行一个自管理 raw 模式的子 TUI（如状态刷新/配置编辑器）：
// 先退出主菜单的 raw，子功能自己 MakeRaw/Restore，结束后恢复主菜单 raw。
func (m *mainMenu) runSub(fn func() error) {
	_ = term.Restore(int(os.Stdin.Fd()), m.oldState)
	if err := fn(); err != nil {
		m.msg = "错误: " + err.Error()
	}
	if _, err := term.MakeRaw(int(os.Stdin.Fd())); err != nil {
		m.msg = "恢复终端失败: " + err.Error()
	}
}

// 实时状态（菜单 1）已合并展示配置信息，原“查看配置列表”菜单已移除。

// doAndWait 执行一个动作并显示结果，按任意键返回主菜单。
func (m *mainMenu) doAndWait(title string, fn func() error) {
	m.enterCooked()
	fmt.Print("\x1b[2J\x1b[H")
	fmt.Println("── " + title + " ──")
	if err := fn(); err != nil {
		fmt.Println("失败:", err)
	} else {
		fmt.Println("完成")
	}
	fmt.Println()
	fmt.Println("（按任意键返回主菜单）")
	if !m.leaveCooked() {
		return
	}
	_, _ = readKey()
}

// ---------- 渲染 ----------

func (m *mainMenu) render() {
	var sb strings.Builder
	sb.WriteString("\x1b[H")
	sb.WriteString("nexthop — 网关下一跳自动切换服务")
	sb.WriteString("\x1b[K\r\n")
	sb.WriteString("  ↑/↓ 选择  回车 执行（或按数字 1-9）  q/ESC 退出")
	sb.WriteString("\x1b[K\r\n")
	sb.WriteString("────────────────────────────────────────")
	sb.WriteString("\x1b[K\r\n")
	for i, item := range menuItems {
		line := fmt.Sprintf("  %d. %s", i+1, item.label)
		if i == m.sel {
			line = "\x1b[7m" + line + "\x1b[0m"
		}
		sb.WriteString(line)
		sb.WriteString("\x1b[K\r\n")
	}
	sb.WriteString("────────────────────────────────────────")
	sb.WriteString("\x1b[K\r\n")
	sb.WriteString(m.msg)
	sb.WriteString("\x1b[K\x1b[J")
	fmt.Print(sb.String())
}
