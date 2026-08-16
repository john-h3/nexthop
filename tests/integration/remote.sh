#!/bin/bash
# nexthop 远程集成测试（在 alpine 测试机上以 root 执行）。
#
# 安全设计：所有路由切换都发生在独立 network namespace（nexthoptest）内，
# 绝不动测试机默认 netns 的 main 路由表，SSH 连接不受影响。
# 探测目标 = 默认 netns 中 veth0 的多 IP 别名上的 mock 服务。
set -euo pipefail

REMOTE=/tmp/nexthop-it
BIN=$REMOTE/nexthop
NS=nexthoptest
SOCK=$REMOTE/nexthop.sock
PASS=0
FAIL=0

say() { echo; echo "== $* =="; }
ok()  { echo "  PASS: $*"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL: $*"; FAIL=$((FAIL + 1)); }

# 说明：测试机可能运行在容器内，ip netns exec 会因尝试 mount /sys 失败；
# 因此统一用 nsenter --net=/var/run/netns/$NS 进入 netns（无需 mount）。
route_gw() { nsenter --net="/var/run/netns/$NS" ip route show default 2>/dev/null | awk '{print $3}'; }

wait_route() {
    local want="$1" tries="${2:-30}" got=""
    for _ in $(seq 1 "$tries"); do
        got=$(route_gw || true)
        [ "$got" = "$want" ] && return 0
        sleep 1
    done
    echo "      等待路由超时：期望 $want，实际 $got"
    return 1
}

mock_state() { curl -sf -X POST -d "$1" http://127.0.0.1:9100/state >/dev/null; }

# 写入初始配置（egress 指向 netns 内网卡 veth1）
write_config() {
    cat >"$REMOTE/config.yaml" <<'YAML'
probe_interval: 1s
probe_timeout: 300ms
egress_device: veth1
stable_rounds: 2
final_ip: 10.99.0.254
targets:
  - name: a
    ip: 10.99.0.11
    weight: 100
    probe: tcp
    port: 9001
  - name: b
    ip: 10.99.0.12
    weight: 80
    probe: http
    url: http://10.99.0.12:9002/healthz
  - name: c
    ip: 10.99.0.13
    weight: 60
    probe: udp
    port: 9003
YAML
}

cleanup() {
    set +e
    rc-service nexthop stop >/dev/null 2>&1
    pkill -f "$BIN run" 2>/dev/null
    pkill -f "$REMOTE/mockupstream" 2>/dev/null
    ip netns del "$NS" 2>/dev/null
    ip link del veth0 2>/dev/null
    rm -f "$SOCK"
}
trap cleanup EXIT

# ---- 准备 netns + veth ----
say "准备 netns ($NS) 与 veth 对"
ip netns del "$NS" 2>/dev/null || true
ip link del veth0 2>/dev/null || true
ip netns add "$NS"
ip link add veth0 type veth peer name veth1
ip link set veth1 netns "$NS"
ip addr add 10.99.0.1/24 dev veth0
ip link set veth0 up
ip addr add 10.99.0.11/32 dev veth0   # tcp mock
ip addr add 10.99.0.12/32 dev veth0   # http mock
ip addr add 10.99.0.13/32 dev veth0   # udp mock
nsenter --net="/var/run/netns/$NS" ip addr add 10.99.0.2/24 dev veth1
nsenter --net="/var/run/netns/$NS" ip link set lo up
nsenter --net="/var/run/netns/$NS" ip link set veth1 up
ok "netns 就绪"

# ---- 启动 mock 上游（默认 netns）----
say "启动 mock 上游"
nohup "$REMOTE/mockupstream" \
    -tcp 10.99.0.11:9001 -http 10.99.0.12:9002 -udp 10.99.0.13:9003 \
    -ctl 127.0.0.1:9100 >"$REMOTE/mock.log" 2>&1 &
sleep 1
if curl -sf http://127.0.0.1:9100/state >/dev/null; then ok "mock 在线"; else bad "mock 启动失败"; exit 1; fi

# ---- 启动 nexthop（netns 内，单二进制 run 子命令）----
write_config
say "启动 nexthop run（netns=$NS）"
nohup nsenter --net="/var/run/netns/$NS" "$BIN" run \
    -c "$REMOTE/config.yaml" -s "$SOCK" \
    >"$REMOTE/nexthop.log" 2>&1 &
sleep 2
if [ -S "$SOCK" ]; then ok "control socket 就绪"; else bad "control socket 未创建"; exit 1; fi

# ---- 场景 1-5：tcp/http/udp 探测与权重切换 ----
say "场景1: 全部 up → 权重最高 a (10.99.0.11)"
if wait_route 10.99.0.11; then ok "默认路由 → 10.99.0.11"; else bad "期望 10.99.0.11"; fi

say "场景2: tcp 挂 → 切到 b (10.99.0.12)"
mock_state '{"tcp":false}'
if wait_route 10.99.0.12; then ok "默认路由 → 10.99.0.12"; else bad "期望 10.99.0.12"; fi

say "场景3: http 挂 → 切到 c (10.99.0.13)"
mock_state '{"http":false}'
if wait_route 10.99.0.13; then ok "默认路由 → 10.99.0.13"; else bad "期望 10.99.0.13"; fi

say "场景4: udp 挂 → 全部失效 → final (10.99.0.254)"
mock_state '{"udp":false}'
if wait_route 10.99.0.254; then ok "默认路由 → final 10.99.0.254"; else bad "期望 10.99.0.254"; fi

say "场景5: 全部恢复 → 回到 a"
mock_state '{"tcp":true,"http":true,"udp":true}'
if wait_route 10.99.0.11; then ok "默认路由 → 10.99.0.11"; else bad "期望 10.99.0.11"; fi

# ---- 场景 6-7：ping 探测 + 热加载 ----
say "场景6: 热加载新增 ping 目标 p (10.99.0.1, weight=200) → 切到 p"
cat >"$REMOTE/config.yaml" <<'YAML'
probe_interval: 1s
probe_timeout: 300ms
egress_device: veth1
stable_rounds: 2
final_ip: 10.99.0.254
targets:
  - name: a
    ip: 10.99.0.11
    weight: 100
    probe: tcp
    port: 9001
  - name: b
    ip: 10.99.0.12
    weight: 80
    probe: http
    url: http://10.99.0.12:9002/healthz
  - name: c
    ip: 10.99.0.13
    weight: 60
    probe: udp
    port: 9003
  - name: p
    ip: 10.99.0.1
    weight: 200
    probe: ping
YAML
"$BIN" reload -s "$SOCK" >/dev/null
if wait_route 10.99.0.1; then ok "默认路由 → ping 目标 10.99.0.1"; else bad "期望 10.99.0.1"; fi

say "场景7: 热加载把 ping 目标改为不可达 (10.99.0.254) → ping 超时 down → 回 a"
sed -i 's/ip: 10.99.0.1$/ip: 10.99.0.254/' "$REMOTE/config.yaml"
"$BIN" reload -s "$SOCK" >/dev/null
if wait_route 10.99.0.11; then ok "默认路由 → 10.99.0.11"; else bad "期望 10.99.0.11"; fi

# ---- 场景 8：控制命令端到端 ----
say "场景8: status/list/add 端到端"
"$BIN" status -s "$SOCK" | tee "$REMOTE/status.out"
if grep -q "10.99.0.11" "$REMOTE/status.out"; then ok "status 显示当前路由"; else bad "status 缺路由"; fi
if grep -q "up" "$REMOTE/status.out"; then ok "status 显示目标状态"; else bad "status 缺目标状态"; fi

"$BIN" list -s "$SOCK" | tee "$REMOTE/list.out"
if grep -q "p" "$REMOTE/list.out"; then ok "list 显示全部目标"; else bad "list 缺目标"; fi

# status 默认在 TTY 下自动进入刷新模式；非 TTY（此处管道环境）自动退化为单次输出，
# 上面的 status | tee 检查已覆盖非 TTY 单次行为。

# nexthop config TUI 编辑器：非 TTY 环境下应给出明确提示
"$BIN" config -s "$SOCK" >"$REMOTE/cfg.out" 2>&1 || true
if grep -q "需要终端" "$REMOTE/cfg.out"; then ok "config 非 TTY 给出提示"; else bad "config TTY 检测异常"; fi

say "场景8b: 交互式 add 新增上游 d (tcp 10.99.0.14:9004)"
printf 'd\n10.99.0.14\n50\ntcp\n9004\ny\n' | "$BIN" add -s "$SOCK"
if "$BIN" list -s "$SOCK" | grep -q "d"; then ok "add 后 list 出现 d"; else bad "add 失败"; fi
# 配置文件应已持久化
if grep -q "10.99.0.14" "$REMOTE/config.yaml"; then ok "配置已持久化"; else bad "配置未持久化"; fi

# ---- 场景 9：nexthop install 自举安装为 OpenRC 服务 ----
say "场景9: nexthop install 安装为 OpenRC 服务（daemon 在 netns 内运行）"
command -v rc-service >/dev/null || apk add --no-cache openrc >/dev/null
# 备份用户现有配置：本场景的 install/uninstall 会动 /etc/nexthop，结束后恢复
USER_CFG_BAK=""
if [ -f /etc/nexthop/config.yaml ]; then
    cp /etc/nexthop/config.yaml "$REMOTE/user-config.bak"
    USER_CFG_BAK=1
fi
pkill -f "$BIN run" 2>/dev/null || true
sleep 1
rm -f "$SOCK"

# 用 -prefix 之外的方式：install 默认装系统路径，先卸载旧的
"$BIN" uninstall >/dev/null 2>&1 || true
"$BIN" install 2>&1 | tee "$REMOTE/install.out"
if [ -x /usr/sbin/nexthop ]; then ok "install 复制二进制到 /usr/sbin/nexthop"; else bad "二进制未安装"; fi
if [ -f /etc/init.d/nexthop ]; then ok "install 生成 OpenRC 脚本"; else bad "initd 未生成"; fi
if [ -f /etc/nexthop/config.yaml ]; then ok "install 写入示例配置"; else bad "配置未写入"; fi

# 示例配置是 eth0（默认 netns），替换为 netns 内安全配置，并启用 NEXTHOP_NETNS
cat > /etc/nexthop/config.yaml <<'YAML'
probe_interval: 1s
probe_timeout: 300ms
egress_device: veth1
stable_rounds: 2
final_ip: 10.99.0.254
targets:
  - name: a
    ip: 10.99.0.11
    weight: 100
    probe: tcp
    port: 9001
YAML
printf 'NEXTHOP_NETNS="%s"\n' "$NS" >/etc/conf.d/nexthop

rc-service nexthop start
sleep 3
if [ -S /run/nexthop.sock ]; then ok "OpenRC 启动后 socket 就绪"; else bad "OpenRC 启动失败"; tail -5 /var/log/nexthop.log 2>/dev/null || true; fi

# 清掉 netns 内可能残留的旧默认路由（pkill 手动 daemon 不会删路由），
# 确保 wait_route 匹配的是 OpenRC daemon 真正设置的路由。
nsenter --net="/var/run/netns/$NS" ip route del default 2>/dev/null || true

# 路由应在 netns 内生效（a up → 路由 10.99.0.11）
if wait_route 10.99.0.11; then ok "OpenRC 下路由正确"; else bad "OpenRC 下路由错误"; fi

# status 轮询等待（daemon 启动有防抖时序：首轮 final、第二轮才切 a）
status_ok=0
for _ in $(seq 1 15); do
    if nexthop status 2>/dev/null | grep -q "10.99.0.11"; then status_ok=1; break; fi
    sleep 1
done
if [ "$status_ok" = 1 ]; then ok "OpenRC 下 status 正常"; else bad "status 异常"; fi

# TCP 探测以 RST 关闭：到探测目标端口(:9001)的 TIME_WAIT 应为 0。
# （HTTP 探测等其他目标因 keep-alive 周期重建会用 FIN 正常关闭，
#   可能残留少量 TIME_WAIT，故按目标端口过滤，只验证 TCP 探测本身。）
tw_count=$(nsenter --net="/var/run/netns/$NS" ss -tan state time-wait "( dport = :9001 )" 2>/dev/null | tail -n +2 | wc -l)
if [ "$tw_count" = "0" ]; then ok "TCP 探测无 TIME_WAIT 堆积（RST 关闭生效）"; else bad "到 :9001 发现 $tw_count 个 TIME_WAIT"; fi

rc-service nexthop stop
sleep 1
if [ ! -S /run/nexthop.sock ]; then ok "OpenRC 停止后 socket 已清理"; else bad "停止后 socket 残留"; fi

"$BIN" uninstall 2>&1 | tee "$REMOTE/uninstall.out"
if [ ! -e /usr/sbin/nexthop ] && [ ! -e /etc/init.d/nexthop ]; then ok "uninstall 清理完成"; else bad "uninstall 未清理干净"; fi

# 恢复用户环境：uninstall 已删除二进制/initd/配置，重新 install 装回服务文件，
# 并把用户原有配置覆盖回示例配置之上（若场景开始时存在）。
"$BIN" install >/dev/null 2>&1
if [ -n "$USER_CFG_BAK" ]; then
    cp "$REMOTE/user-config.bak" /etc/nexthop/config.yaml
    echo "已恢复用户原配置: /etc/nexthop/config.yaml"
fi
# 注：不自动启动服务，测试不擅自改变服务运行状态

# ---- 汇总 ----
echo
echo "========================================"
echo " 集成测试结果: PASS=$PASS  FAIL=$FAIL"
echo "========================================"
[ "$FAIL" -eq 0 ]
