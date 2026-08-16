// nexthop 是网关下一跳自动切换服务的单二进制：
//   nexthop run -c config   启动后端服务（前台，由 OpenRC 等管理）
//   nexthop status/list/add/reload   控制命令（经 unix socket 与 daemon 通信）
//   nexthop install/uninstall        自举安装/清理为 OpenRC 系统服务
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	args := os.Args[1:]
	// 无参数（或直接以选项开头）→ 进入 TUI 主菜单；子命令保留供脚本/服务使用。
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		if err := cmdTUI(args); err != nil {
			fmt.Fprintln(os.Stderr, "错误:", err)
			os.Exit(1)
		}
		return
	}

	var err error
	switch args[0] {
	case "run":
		err = cmdRun(args[1:])
	case "status":
		err = cmdStatus(args[1:])
	case "list":
		err = cmdList(args[1:])
	case "add":
		err = cmdAdd(args[1:])
	case "config":
		err = cmdConfig(args[1:])
	case "reload":
		err = cmdReload(args[1:])
	case "install":
		err = cmdInstall(args[1:])
	case "uninstall":
		err = cmdUninstall(args[1:])
	case "version":
		fmt.Println("nexthop " + version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

const version = "0.2.0"

func usage() {
	fmt.Fprint(os.Stderr, `nexthop — 网关下一跳自动切换服务

用法:
  nexthop run -c <config> [-s <socket>]   启动后端服务（前台运行）
  nexthop status                          实时状态（终端下自动刷新，q/ESC 退出；-o 单次）
  nexthop list                            列出配置的所有上游
  nexthop add                             交互式增加新的上游
  nexthop config                           TUI 编辑配置（全局 + 上游）
  nexthop reload                          触发热加载配置（等价 SIGHUP）
  nexthop install                         安装为 OpenRC 系统服务
  nexthop uninstall                       卸载并清理
  nexthop version                         显示版本

控制命令通用选项:
  -s, -socket <path>   control unix socket 路径 (default /run/nexthop.sock)

运行选项:
  -c, -config <path>   配置文件路径 (default /etc/nexthop/config.yaml)
`)
}
