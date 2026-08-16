#!/bin/bash
# 安装验证：把 bin/nexthop 拷到目标机（alpine）后，
# 执行 nexthop install 自举安装为 OpenRC 服务，验证服务生命周期，最后 uninstall 清理。
#
# 安全设计：daemon 通过 /etc/conf.d/nexthop 的 NEXTHOP_NETNS 特性在独立 netns 内运行，
# 绝不修改测试机主路由表（SSH 不受影响）。
set -e

REMOTE=${REMOTE:-/tmp/nx-install}
BIN=$REMOTE/nexthop

echo "=== 1. 清理旧安装 ==="
"$BIN" uninstall >/dev/null 2>&1 || true

echo "=== 2. nexthop install ==="
"$BIN" install

echo "=== 3. 校验文件 ==="
ls -l /usr/sbin/nexthop /etc/init.d/nexthop /etc/nexthop/config.yaml
"$BIN" version

echo "=== 4. 准备 netns 拓扑 ==="
ip netns del nxinstall 2>/dev/null || true
ip link del v0 2>/dev/null || true
ip netns add nxinstall
ip link add v0 type veth peer name v1
ip link set v1 netns nxinstall
ip link set v0 up
nsenter --net=/var/run/netns/nxinstall ip link set v1 up

echo "=== 5. 写入安全配置（netns 内运行） ==="
cat > /etc/nexthop/config.yaml <<'EOF'
probe_interval: 2s
probe_timeout: 1s
egress_device: v1
stable_rounds: 2
final_ip: 10.99.0.254
targets:
  - name: t
    ip: 10.99.0.99
    weight: 100
    probe: tcp
    port: 9001
EOF
echo 'NEXTHOP_NETNS="nxinstall"' > /etc/conf.d/nexthop

# 清理可能的残留 pidfile/socket（上次运行崩溃/失败时可能遗留）
rm -f /run/nexthop.pid /run/nexthop.sock /run/nexthop-netns-wrapper

echo "=== 6. rc-service nexthop start ==="
rc-service nexthop start
sleep 3

echo "=== 7. 验证 socket 与 CLI ==="
ls -l /run/nexthop.sock && echo "SOCKET OK"
nexthop status

echo "=== 8. rc-service nexthop stop ==="
rc-service nexthop stop
sleep 1
if [ ! -e /run/nexthop.sock ]; then echo "STOP CLEAN OK"; else echo "STOP CLEAN FAIL"; exit 1; fi

echo "=== 9. nexthop uninstall ==="
"$BIN" uninstall

echo "=== 10. 清理测试拓扑 ==="
ip netns del nxinstall 2>/dev/null || true
ip link del v0 2>/dev/null || true

echo "ALL DONE"
