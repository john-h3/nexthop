package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"nexthop/internal/config"
	"nexthop/internal/control"
	"nexthop/internal/router"
	"nexthop/internal/state"
)

const (
	defaultConfigPath = "/etc/nexthop/config.yaml"
	defaultSockPath   = "/run/nexthop.sock"
)

// daemonHandler 实现 control.Handler，供 unix socket server 调用。
type daemonHandler struct {
	cfgPath string
	log     *slog.Logger
	mgr     *state.Manager
}

func (d *daemonHandler) Status() ([]state.TargetStatus, string, bool) {
	return d.mgr.Status()
}

func (d *daemonHandler) Config() *config.Config {
	return d.mgr.Config()
}

func (d *daemonHandler) ConfigPath() string { return d.cfgPath }

// ApplyConfig 校验、持久化并热加载新配置（add 命令的落地点）。
func (d *daemonHandler) ApplyConfig(data []byte) error {
	cfg, err := config.Parse(data)
	if err != nil {
		return err
	}
	if err := config.Save(d.cfgPath, cfg); err != nil {
		return err
	}
	return d.mgr.Reload(cfg)
}

// ReloadFromFile 从配置文件重新加载（SIGHUP / reload 命令）。
func (d *daemonHandler) ReloadFromFile() error {
	cfg, err := config.Load(d.cfgPath)
	if err != nil {
		return err
	}
	return d.mgr.Reload(cfg)
}

// cmdRun 启动守护进程（前台运行）。用法：nexthop run -c <config> [-s <socket>]
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("c", defaultConfigPath, "配置文件路径")
	fs.StringVar(cfgPath, "config", defaultConfigPath, "配置文件路径")
	sockPath := fs.String("s", defaultSockPath, "control unix socket 路径")
	fs.StringVar(sockPath, "socket", defaultSockPath, "control unix socket 路径")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("加载配置失败", "err", err)
		return err
	}

	r, err := router.New(cfg.EgressDevice)
	if err != nil {
		log.Error("初始化路由模块失败", "err", err)
		return err
	}

	mgr, err := state.New(cfg, r, log)
	if err != nil {
		log.Error("初始化状态管理器失败", "err", err)
		return err
	}

	h := &daemonHandler{cfgPath: *cfgPath, log: log, mgr: mgr}
	srv := control.NewServer(*sockPath, h, log)

	// 探测循环（立即执行一轮并设置路由）。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	// control server。
	ln, err := srv.Listen()
	if err != nil {
		log.Error("监听 control socket 失败", "err", err)
		return err
	}
	go func() {
		if err := srv.Serve(ln); err != nil {
			log.Error("control server 异常退出", "err", err)
			cancel()
		}
	}()
	log.Info("nexthop 已启动",
		"config", *cfgPath, "socket", *sockPath,
		"egress", cfg.EgressDevice, "targets", len(cfg.Targets))

	// 信号处理：SIGHUP 热加载，SIGTERM/SIGINT 优雅退出。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	for sig := range sigCh {
		switch sig {
		case syscall.SIGHUP:
			log.Info("收到 SIGHUP，重载配置")
			if err := h.ReloadFromFile(); err != nil {
				log.Error("重载配置失败（保留旧配置）", "err", err)
			}
		default:
			log.Info("收到退出信号", "signal", sig.String())
			srv.Shutdown()
			return nil
		}
	}
	return nil
}
