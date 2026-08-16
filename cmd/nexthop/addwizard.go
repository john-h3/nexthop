package main

import (
	"fmt"
	"os"
	"strings"

	"nexthop/internal/config"
	"nexthop/internal/control"

	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// addWizard 是主菜单"新增上游"的独立向导：逐字段输入新上游，完成后确认保存。
// 不进入配置编辑器，避免展示无关的现有配置。
type addWizard struct {
	c   *control.Client
	cfg *config.Config

	taskQueue []inputTask
	taskIdx   int

	mode  editMode
	prompt string
	cur    string
	buf    []byte
	apply  func(string) error

	msg      string
	finished bool // 完成（等待任意键返回）
	quit     bool
}

// newAddWizard 从 daemon 读取当前配置并构造新增向导。
func newAddWizard(c *control.Client) (*addWizard, error) {
	e, err := newCfgEditor(c)
	if err != nil {
		return nil, err
	}
	return &addWizard{c: c, cfg: e.cfg}, nil
}

func (w *addWizard) run() error {
	if !term.IsTerminal(int(os.Stdout.Fd())) || !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("新增上游需要终端（TTY）运行")
	}
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("设置终端原始模式失败: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	w.start()
	w.render()
	for !w.quit {
		ev, err := readKey()
		if err != nil {
			return err
		}
		if w.finished {
			w.quit = true // 完成画面已显示，任意键返回主菜单
			break
		}
		w.handle(ev)
		w.render()
	}
	return nil
}

// start 构建字段序列：基础字段 + 按探测方式追加端口/URL + 确认保存任务。
func (w *addWizard) start() {
	var t config.Target
	w.taskQueue = targetTasks(&t, false)
	// 名字查重
	w.taskQueue[0].apply = func(s string) error {
		s = strings.TrimSpace(s)
		if s == "" {
			return fmt.Errorf("名字不能为空")
		}
		for _, x := range w.cfg.Targets {
			if x.Name == s {
				return fmt.Errorf("名字 %q 已存在", s)
			}
		}
		t.Name = s
		return nil
	}
	done := w.taskQueue
	w.taskQueue = append(done, inputTask{label: "确认保存 (y/N)", cur: "y", apply: func(s string) error {
		if !strings.EqualFold(strings.TrimSpace(s), "y") {
			return fmt.Errorf("已取消")
		}
		w.cfg.Targets = append(w.cfg.Targets, t)
		if err := w.cfg.Validate(); err != nil {
			return fmt.Errorf("配置校验失败: %w", err)
		}
		data, err := yaml.Marshal(w.cfg)
		if err != nil {
			return fmt.Errorf("序列化失败: %w", err)
		}
		if err := w.c.PostConfig(data); err != nil {
			return fmt.Errorf("保存失败: %w", err)
		}
		w.msg = "已新增上游 " + t.Name + " 并保存生效"
		w.finished = true
		return nil
	}})
	w.taskIdx = 0
	w.nextTask()
}

func (w *addWizard) nextTask() {
	for w.taskIdx < len(w.taskQueue) {
		t := w.taskQueue[w.taskIdx]
		w.taskIdx++
		if t.skip != nil && t.skip() {
			continue // 按条件跳过（如探测方式不匹配的端口/URL 任务）
		}
		w.mode = modeInput
		w.prompt = t.label
		w.cur = t.cur
		w.buf = nil
		w.apply = t.apply
		return
	}
	w.quit = true
}

func (w *addWizard) handle(ev keyEvent) {
	if w.mode != modeInput {
		return
	}
	switch ev.act {
	case actChar:
		if ev.ch == 'q' {
			w.quit = true
			w.msg = "已取消"
			return
		}
		w.buf = append(w.buf, ev.ch)
	case actBackspace:
		if len(w.buf) > 0 {
			w.buf = w.buf[:len(w.buf)-1]
		}
	case actEnter:
		val := string(w.buf)
		if val == "" {
			val = w.cur
		}
		if err := w.apply(val); err != nil {
			w.msg = "无效输入: " + err.Error()
			w.buf = nil
			return
		}
		w.nextTask()
	case actEsc:
		w.quit = true
		w.msg = "已取消"
	}
}

func (w *addWizard) render() {
	var sb strings.Builder
	sb.WriteString("\x1b[H")
	sb.WriteString("── 新增上游 ──")
	sb.WriteString("\x1b[K\r\n")
	if w.finished {
		sb.WriteString(w.msg)
		sb.WriteString("\x1b[K\r\n")
		sb.WriteString("（按任意键返回主菜单）")
	} else if w.mode == modeInput {
		sb.WriteString(w.prompt + " [" + w.cur + "]: " + string(w.buf))
	} else {
		sb.WriteString(w.msg)
	}
	sb.WriteString("\x1b[K\x1b[J")
	fmt.Print(sb.String())
}
