# dae (itoywh fork)

> Fork from [daeuniverse/dae](https://github.com/daeuniverse/dae) with custom patches for enhanced node management.

<p align="left">
    <img src="https://github.com/itoywh/dae/actions/workflows/build.yml/badge.svg" alt="Build"/>
    <img src="https://img.shields.io/github/v/release/itoywh/dae?include_prereleases" alt="Release"/>
    <img src="https://img.shields.io/github/license/daeuniverse/dae?color=orange" alt="License"/>
</p>

## v2.0-custom

基于 dae v2.0.0rc1 的自定义构建，包含以下增强补丁：

| 补丁 | 说明 |
|------|------|
| **P2** | FixedWithFallback 增强：修复延迟不跟踪 bug，新增 timeout+retry 重试 |
|      | 紧急探针主动检测（最小间隔 2s）；纳秒级超时精度；互斥锁保护重试状态 |
|      | `retries=0` 时立即 fallback，不等待 timeout |
| **P3** | 健康检查完全配置化：移除默认值 opt-in；禁用时输出 WARN 日志提醒 |
| **P4** | 节点状态日志：DEAD/ALIVE 变更输出 |
| **P5** | 日志时区：CST (Asia/Shanghai)；使用 cstFormatter 避免修改全局 time.Local |
| **P6** | veth fallback：`DAE_DISABLE_NETKIT=1` 环境变量强制走 veth（解决 #1024 接口漂移） |

**分支**: `v2.0-custom` · **预编译二进制**: [Releases](https://github.com/itoywh/dae/releases) · **PR**: [#1](https://github.com/itoywh/dae/pull/1) · **Issues**: [#1024](https://github.com/daeuniverse/dae/issues/1024)

> 补丁 P1-P5 仅修改 dialer/config 层。P6 修改 `control/netns_utils.go`，涉及 eBPF 设备层。

### P2 详细说明

**FixedWithFallback 策略语法**：`fixed_fallback(index, timeout, retries, fallback_policy)`

**参数说明**：
- `index`: 固定节点索引
- `timeout`: 超时时间（支持亚秒级，如 `500ms`）
- `retries`: 重试次数
  - `retries=0`: 节点 dead 后立即 fallback，不等待 timeout
  - `retries>0`: 每次 timeout 后发送紧急探针检测，最多重试 retries 次
- `fallback_policy`: 备用策略（`random`、`min_last_latency` 等）

**紧急探针机制**：
- 节点 dead 后，每次 timeout 到达时主动发送 TCP/UDP 探针检测是否恢复
- 探针最小间隔 **2s**（cooldown），防止探针风暴保护系统资源
- 为避免请求长时间堵塞，建议 `timeout >= 2s`（实际间隔为 `max(2s, timeout)`）
- 探针成功 → 立即恢复使用固定节点
- 重试耗尽 → fallback 到备用策略

### P6 详细说明

**痛点**：dae v2.0 在 kernel 6.7+ 上默认使用 netkit 设备对，但 netkit 接口索引可能在运行时漂移（issue [#1024](https://github.com/daeuniverse/dae/issues/1024)），导致 eBPF redirect 失败，翻墙流量返回 `Operation not permitted`。

**修复**：通过 `DAE_DISABLE_NETKIT=1` 环境变量强制走 veth（稳定，更成熟的设备类型）。

**使用方式**：
```bash
# procd（ImmortalWrt/OpenWrt）：在 /etc/init.d/dae 的 procd_set_param env 行追加
procd_set_param env DAE_LOCATION_ASSET="/usr/share/v2ray" DAE_DISABLE_NETKIT=1

# systemd：在 service 文件中添加
Environment=DAE_DISABLE_NETKIT=1

# CLI 启动
DAE_DISABLE_NETKIT=1 /usr/bin/dae run --config /etc/dae/config.dae
```

**恢复 netkit**：去掉该变量或设为 `0` 即可。

**性能差异**：veth 比 netkit 多几字节 ethernet header 拷贝，实际影响 < 1%，可忽略。

---

## 关于 dae (upstream)

**_dae_**, means goose, is a high-performance transparent proxy solution.

To enhance traffic split performance as much as possible, dae employs the transparent proxy and traffic split suite within the Linux kernel using eBPF. As a result, dae can enable direct traffic to bypass the proxy application's forwarding, facilitating genuine direct traffic passage. Through this remarkable feat, there is minimal performance loss and negligible additional resource consumption for direct traffic. 

As a successor of [v2rayA](https://github.com/v2rayA/v2rayA), dae abandoned v2ray-core to meet the needs of users more freely.

## Features

- [x] Implement `Real Direct` traffic split (need ipforward on) to achieve [high performance](https://docs.google.com/spreadsheets/d/1UaWU6nNho7edBNjNqC8dfGXLlW0-cm84MM7sH6Gp7UE/edit?usp=sharing).
- [x] Support to split traffic by process name in local host.
- [x] Support to split traffic by MAC address in LAN.
- [x] Support to split traffic with invert match rules.
- [x] Support to automatically switch nodes according to policy. That is to say, support to automatically test independent TCP/UDP/IPv4/IPv6 latencies, and then use the best nodes for corresponding traffic according to user-defined policy.
- [x] Support advanced DNS resolution process.
- [x] Support full-cone NAT for shadowsocks, trojan(-go) and socks5 (no test).
- [x] Support various trending proxy protocols, seen in [proxy-protocols.md](./docs/en/proxy-protocols.md).

## Getting Started

Please refer to [Quick Start Guide](./docs/en/README.md) to start using `dae` right away!

## Notes

1. If you setup dae and also a shadowsocks server (or any UDP servers) on the same machine in public network, such as a VPS, don't forget to add `l4proto(udp) && sport(your server ports) -> must_direct` rule for your UDP server port. Because states of UDP are hard to maintain, all outgoing UDP packets will potentially be proxied (depends on your routing), including traffic to your client. This behaviour is not what we want to see. `must_direct` makes all traffic from this port including DNS traffic direct.
1. If users in mainland China find that the first screen time is very long when they visit some domestic websites for the first time, please check whether you use foreign DNS to handle some domestic domain in DNS routing. Sometimes this is hard to spot. For example, `ocsp.digicert.cn` is included in `geosite:geolocation-!cn` unexpectedly, which will cause some tls handshakes to take a long time. Be careful to use such domain sets in DNS routing.

## How it works

See [How it works](./docs/en/how-it-works.md).

## TODO

- [ ] Automatically check dns upstream and source loop (whether upstream is also a client of us) and remind the user to add sip rule.
- [ ] MACv2 extension extraction.
- [ ] Log to userspace.
- [ ] Protocol-oriented node features detecting (or filter), such as full-cone (especially VMess and VLESS).
- [ ] Add quick-start guide
- [ ] ...

## Contributors

Special thanks goes to all [contributors](https://github.com/daeuniverse/dae/graphs/contributors). If you would like to contribute, please see the [instructions](./docs/en/development/contribute.md). Also, it is recommended following the [commit-msg-guide](./docs/en/development/commit-msg-guide.md).

## License

[AGPL-3.0 (C) daeuniverse](https://github.com/daeuniverse/dae/blob/main/LICENSE)

## Stargazers over time

[![Stargazers over time](https://starchart.cc/daeuniverse/dae.svg)](https://starchart.cc/daeuniverse/dae)
