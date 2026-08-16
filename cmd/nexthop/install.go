package main

import (
	_ "embed"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

//go:embed example.yaml
var exampleConfig []byte

// installPaths 保存安装路径（默认系统路径，-prefix 可覆盖用于测试）。
type installPaths struct {
	bin     string // 二进制（/usr/sbin/nexthop）
	initd   string // OpenRC 脚本（/etc/init.d/nexthop）
	confDir string // 配置目录（/etc/nexthop）
	confd   string // OpenRC 变量（/etc/conf.d/nexthop）
	sock    string // control socket
	log     string // 日志
}

func defaultInstallPaths(prefix string) installPaths {
	if prefix != "" {
		prefix = strings.TrimSuffix(prefix, "/")
	}
	return installPaths{
		bin:     prefix + "/usr/sbin/nexthop",
		initd:   prefix + "/etc/init.d/nexthop",
		confDir: prefix + "/etc/nexthop",
		confd:   prefix + "/etc/conf.d/nexthop",
		sock:    "/run/nexthop.sock",
		log:     "/var/log/nexthop.log",
	}
}

// cmdInstall 自举安装：复制自身到系统路径、生成 OpenRC 脚本、写入示例配置。
// 用法：nexthop install [-prefix <dir>]
func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	prefix := fs.String("prefix", "", "安装前缀（默认系统路径；主要用于测试/打包）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p := defaultInstallPaths(*prefix)

	if err := requireRoot("install"); err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取当前可执行文件路径: %w", err)
	}

	// 1. 复制自身（已在目标路径则跳过）。
	if err := os.MkdirAll(filepath.Dir(p.bin), 0o755); err != nil {
		return err
	}
	if self != p.bin {
		if err := copyFile(self, p.bin, 0o755); err != nil {
			return fmt.Errorf("安装二进制到 %s: %w", p.bin, err)
		}
		fmt.Printf("已安装二进制: %s\n", p.bin)
	} else {
		fmt.Printf("二进制已在目标路径，跳过复制: %s\n", p.bin)
	}

	// 2. 生成 OpenRC 脚本。
	if err := os.MkdirAll(filepath.Dir(p.initd), 0o755); err != nil {
		return err
	}
	initd := renderInitd(p)
	if err := os.WriteFile(p.initd, []byte(initd), 0o755); err != nil {
		return fmt.Errorf("写入 OpenRC 脚本: %w", err)
	}
	fmt.Printf("已安装 OpenRC 脚本: %s\n", p.initd)

	// 3. 示例配置（已存在则不覆盖）。
	if err := os.MkdirAll(p.confDir, 0o755); err != nil {
		return err
	}
	confPath := p.confDir + "/config.yaml"
	if _, err := os.Stat(confPath); os.IsNotExist(err) {
		if err := os.WriteFile(confPath, exampleConfig, 0o644); err != nil {
			return fmt.Errorf("写入示例配置: %w", err)
		}
		fmt.Printf("已安装示例配置: %s（请按实际环境修改）\n", confPath)
	} else {
		fmt.Printf("保留现有配置: %s\n", confPath)
	}

	// 4. 注册开机自启并启动服务（仅系统安装；-prefix 场景跳过）。
	if *prefix == "" {
		if out, err := exec.Command("rc-update", "add", "nexthop", "default").CombinedOutput(); err != nil {
			fmt.Printf("警告：注册开机自启失败: %v\n%s", err, out)
		} else {
			fmt.Println("已注册开机自启")
		}
		if out, err := exec.Command("rc-service", "nexthop", "start").CombinedOutput(); err != nil {
			fmt.Printf("警告：启动服务失败: %v\n%s", err, out)
		} else {
			fmt.Println("已启动服务")
		}
	} else {
		fmt.Println("跳过服务注册与启动（-prefix 模式）")
	}

	fmt.Println("可选：/etc/conf.d/nexthop 设置 NEXTHOP_NETNS=<ns> 在指定 netns 内运行")
	return nil
}

// cmdUninstall 停止服务并删除全部安装文件。
// 用法：nexthop uninstall [-prefix <dir>]
func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	prefix := fs.String("prefix", "", "安装前缀（默认系统路径；与 install 对应）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p := defaultInstallPaths(*prefix)

	if err := requireRoot("uninstall"); err != nil {
		return err
	}

	// 停服务（仅当 OpenRC 可用）。
	if _, err := os.Stat("/etc/init.d/nexthop"); err == nil {
		runShell("rc-service nexthop stop 2>/dev/null || true")
		runShell("rc-update del nexthop 2>/dev/null || true")
	}

	removed := []string{p.bin, p.initd, p.confDir, p.confd, p.sock, "/run/nexthop.pid", "/run/nexthop-netns-wrapper"}
	for _, path := range removed {
		if err := os.RemoveAll(path); err == nil {
			fmt.Printf("已删除: %s\n", path)
		}
	}
	fmt.Println("nexthop 已卸载")
	return nil
}

func requireRoot(cmd string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("%s 需要 root 权限（sudo %s）", cmd, cmd)
	}
	return nil
}

// copyFile 以"临时文件 + rename"原子替换方式安装二进制。
// 直接 O_TRUNC 覆盖正在执行的 ELF 会触发 ETXTBSY（text file busy），
// 而 rename 覆盖运行中的文件是允许的：运行中进程继续用旧 inode，
// 服务重启后使用新版本。
func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".nexthop-install-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// renderInitd 生成 OpenRC 脚本（单二进制：nexthop run）。
func renderInitd(p installPaths) string {
	return fmt.Sprintf(`#!/sbin/openrc-run
# nexthop 网关守护进程的 OpenRC 服务脚本（由 nexthop install 生成）

name="nexthop"
description="Nexthop gateway service: probe upstreams and manage default route"
command="%s"
command_args="run -c %s/config.yaml -s %s"
command_background="yes"
pidfile="/run/nexthop.pid"
output_log="%s"
error_log="%s"

NEXTHOP_NETNS="${NEXTHOP_NETNS:-}"

depend() {
    need net
}

start_pre() {
    checkpath -d -m 0755 %s
    if [ -n "$NEXTHOP_NETNS" ]; then
        ebegin "在 network namespace ${NEXTHOP_NETNS} 中启动 nexthop"
        # busybox start-stop-daemon 会把 nsenter 的 --net= 误解析为自己的选项，
        # 因此通过 wrapper 脚本间接启动（exec 链保持同一 pid，OpenRC 可正常管理）。
        cat >/run/nexthop-netns-wrapper <<WRAPPER
#!/bin/sh
exec /usr/bin/nsenter --net=/var/run/netns/${NEXTHOP_NETNS} %s run -c %s/config.yaml -s %s "\$@"
WRAPPER
        chmod 0755 /run/nexthop-netns-wrapper
        command="/run/nexthop-netns-wrapper"
    fi
}

stop_post() {
    rm -f %s /run/nexthop.pid /run/nexthop-netns-wrapper
}
`,
		p.bin, p.confDir, p.sock, p.log, p.log,
		p.confDir,
		p.bin, p.confDir, p.sock,
		p.sock,
	)
}

// runShell 执行一个简单 shell 命令（忽略输出，仅用于 uninstall 清理）。
func runShell(cmd string) {
	// 使用 sh -c 执行；失败静默（如 rc-service 不存在）。
	_ = execSh(cmd)
}

// execSh 通过 /bin/sh -c 执行命令并丢弃输出。
func execSh(cmd string) error {
	return exec.Command("/bin/sh", "-c", cmd).Run()
}
