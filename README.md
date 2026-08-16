# nexthop — 网关下一跳自动切换服务

nexthop 是一个运行在网关机器上的 Go 守护进程：周期探测多个上游（下一跳）的连通性，
从存活上游中按权重选择最高者，通过 **netlink 系统调用**（不调用任何命令行工具）
把它设为机器的 IPv4 默认路由。所有到达本机的流量因此自动走"当前最健康"的上游。

## 功能

- 四种连通性探测：`ping`（仅 IP）、`tcp` / `udp`（IP + 端口）、`http`（完整 URL，跳过 TLS 校验）
- 探测间隔可配置，支持 `1ms` / `1s` / `1m` 等 Go duration 写法
- 权重决策：存活目标按权重降序选择，同权重保持配置顺序
- 防抖：目标状态需连续 `stable_rounds` 轮探测保持才翻转，避免路由 flapping
- 兜底：全部上游失效时，路由切到 `final_ip`
- 热加载：`SIGHUP` 或 `nexthop reload`
- 本地管理通道：Unix domain socket + HTTP（`/run/nexthop.sock`），不暴露网络端口
- 自举安装：`nexthop install` 一键安装为 OpenRC 系统服务，`uninstall` 完整清理

## 构建

单二进制，无 Makefile：

```sh
go build ./cmd/nexthop                          # 当前架构（bin/nexthop）
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build ./cmd/nexthop                        # 交叉编译 arm64（如树莓派 alpine）
go test ./...                                   # 单元测试（无需特权）
```

产物为 `CGO_ENABLED=0` 静态二进制，debian 编译、alpine 直接运行。

## 用法

```
nexthop run -c <config> [-s <socket>]   启动后端服务（前台，由 OpenRC 管理）
nexthop status                          查看实时状态（目标健康度、当前生效上游）
nexthop list                            列出配置的所有上游
nexthop add                             交互式增加新的上游（经 daemon 校验、持久化并热加载）
nexthop reload                          触发热加载配置（等价 SIGHUP）
nexthop install                         安装为 OpenRC 系统服务
nexthop uninstall                       卸载并清理
nexthop version                         显示版本
```

## 安装（OpenRC，alpine）

```sh
# 把编译好的 bin/nexthop 拷到目标机，然后：
nexthop install
rc-service nexthop start
rc-update add nexthop default          # 注册开机自启
```

`nexthop install` 会：复制自身到 `/usr/sbin/nexthop`、生成 OpenRC 脚本 `/etc/init.d/nexthop`、
写入示例配置 `/etc/nexthop/config.yaml`（已存在则不覆盖）。
`nexthop uninstall` 停止服务并删除全部文件。
非 root 执行时会提示需要 root（或 sudo）。

服务需要 root（改路由需要 `CAP_NET_ADMIN`，ping 需要 `CAP_NET_RAW`）。
可选：在 `/etc/conf.d/nexthop` 设置 `NEXTHOP_NETNS="<ns>"` 把 daemon 放进指定
network namespace 运行（适合容器/受限环境，不影响默认网络命名空间）。

## 部署（Docker）

```sh
docker build -f deploy/Dockerfile -t nexthop .
docker run -d --name nexthop \
  --network host --cap-add NET_ADMIN --cap-add NET_RAW \
  -v /etc/nexthop/config.yaml:/etc/nexthop/config.yaml:ro \
  nexthop
```

## 配置

见 [`cmd/nexthop/example.yaml`](cmd/nexthop/example.yaml)（已嵌入二进制，由 `nexthop install` 写入），
部署到 `/etc/nexthop/config.yaml`：

```yaml
probe_interval: 5s
probe_timeout: 1s
egress_device: eth0      # 出口网卡，default route 绑定的 dev
stable_rounds: 2
final_ip: 10.0.0.254     # 全部上游失效时的兜底

targets:
  - name: aliyun
    ip: 10.0.0.1
    weight: 100
    probe: ping
  - name: tencent
    ip: 10.0.0.2
    weight: 80
    probe: tcp
    port: 443
```

## 测试

- 单元测试：`go test ./...`（debian/alpine 均可，无需特权）
- 集成测试需要一台具备 root、OpenRC、网络命名空间和 SSH 访问权限的 Alpine
  测试机。`tests/integration/remote.sh` 负责测试机上的隔离测试流程；请使用者
  自行编写本地 SSH 编排入口并设置目标主机和部署路径，避免提交环境相关信息。
- 安装验证：`tests/integration/verify-install.sh`（把 `bin/nexthop` 放到 `/tmp/nx-install/` 后执行）

## 架构

```
cmd/nexthop      单二进制（run 守护进程 / status|list|add|reload 控制 / install|uninstall 自举）
internal/config  配置解析 / 校验 / 原子持久化
internal/probe   四种探测器（ping/tcp/udp/http）
internal/state   探测循环、防抖状态机、权重决策、final 兜底
internal/router  netlink 默认路由管理（绑定 egress_device）
internal/control unix socket HTTP 服务端 + 客户端
tests/           集成测试（mock 上游 + netns 隔离编排）
```
