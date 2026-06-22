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

### P2 FixedWithFallback 详细说明

**策略语法**：`fixed_fallback(index, timeout, retries, fallback_policy)`

**参数说明**：
- `index`: 固定节点索引（从0开始，严格按 `node {}` 书写顺序）
- `timeout`: 单次重试间隔（支持亚秒级，如 `500ms`；低于 2s 自动抬升到 2s 并输出 WARN 日志，防止探针风暴）
- `retries`: 最大重试次数
  - `retries=0`: 节点 dead 后立即 fallback，不等待 timeout
  - `retries>0`: 后台 goroutine 按 timeout 间隔驱动重试探测
- `fallback_policy`: 备用选择策略（`random`、`min_last_latency`、`min_moving_avg_latency` 等）

#### 工作原理

```
                    ┌─ MustGetAlive=true  ── 正常返回固定节点 (s4)
                    │
  Select() ─────────┤                                   自然流量
                    │                                     ↓
                    └─ MustGetAlive=false ── → goto doFallback → 备份节点 (s2/s5)
                                               │
                                               ├─ 启动后台 goroutine
                                               │     │
                                               │     ├─ ticker 每 timeout ─┬─ 探测成功 → deadSince=0 → 流量切回固定节点
                                               │     │                      └─ 失败 → retryCount++
                                               │     │                                   └─ ≥retries → 永久 fallback
                                               │     │
                                               │     └─ 发送 NotifyCheckTcp/Udp 探针触发健康检查
                                               │
                                               └─ 后续 Select() → deadSince≠0 → goto doFallback
```

#### 核心设计：两层分离

| 层 | 职责 | 是否向死节点发流量 |
|----|------|------------------|
| **自然流量**（`Select()`） | MustGetAlive=false 立刻 fallback；MustGetAlive=true 立刻切回 | ❌ 从不（从第一个字节开始就走备份） |
| **后台 goroutine** | 独立 ticker 驱动重试；发送探针检测固定节点是否恢复 | ✅ 探针专用（仅发健康检查请求） |

#### 四个关键事件流

**① 节点从活→死**
```
健康检查探测失败 → Alive=false
  ├─ [路径A] aliveTransitionCallback → CompareAndSwap(false→true) → 启动 goroutine
  └─ [路径B] 自然流量 Select() 发现 MustGetAlive=false
       └─ deadSince==0 → 记录时间 → 若 goroutine 未启动则启动之 → goto doFallback
```

**② 死亡期间的自然流量**
```
Select() → MustGetAlive=false → deadSince≠0 → goto doFallback → 返回 s2/s5
```
所有自然流量绕过死节点，**不参与重试计数**。

**③ 后台重试探测**
```
goroutine ticker 触发:
  └─ MustGetAlive? ─┬─ true  → deadSince=0 → return（goroutine 退出）
                      └─ false → retryCount++
                           ├─ retryCount < retries → 等待下一个 tick
                           └─ retryCount ≥ retries → 标记永久 fallback → return
```
`NotifyCheckTcp()` 和 `NotifyCheckDnsUdp()` 在每个 tick 发送探针。

**④ 节点从死→活**
```
恢复探测触发（健康检查或 goroutine 探针）→ MustGetAlive=true
  ├─ goroutine 检测到 → deadSince=0, retryCount=0 → return
  └─ 自然流量 Select() 检测到 → deadSince=0, retryCount=0 → 返回固定节点
```
**切回零延迟**：恢复发生在健康检查周期内（如 `check_interval=10s` 最多等 10s），与 goroutine 的 ticker 无关。

#### 零延迟切换对比

| 方案 | 死了→fallback | 活了→切回 | 死节点流量 |
|------|-------------|----------|----------|
| SSR-Plus | 60s 循环检测后 | 每循环检测主力 | 60s |
| PassWall 2 | 1min burstObservatory | 3次均值判定 | 1-3min |
| **dae（本补丁）** | **第一次流量即 fallback** | **健康检查周期内恢复** | **0（零自然流量浪费）** |

#### 关键实现细节

- **aliveTransitionCallback**：健康检查（connectivity_check）标记 TCP dead 时自动启动 goroutine，不依赖自然流量触发
- **deadSince 时间戳**：`fixedFallbackDeadSince` 记录首次发现死亡的时刻，用于区分首次 vs 后续 Select()
- **CompareAndSwap**：保证仅一个 goroutine 在运行，`fixedFallbackStopCh` 用于 goroutine 退出时信号传递
- **探针 cooldown**：timeout 或 check_interval 低于 2s 时自动抬升到 2s 并输出 WARN 日志，防止探针风暴
- **恢复后清除状态**：`MustGetAlive=true` 同时清除 `deadSince` 和 `retryCount`，确保状态干净
- **支持 UDP-only 检测**：不限于 TCP 类型的健康检查，UDP/DNS-UDP/Data-UDP 的 dead transition 同样触发 goroutine

<details>
<summary><b>P2 FixedWithFallback — English</b></summary>

**Syntax**: `fixed_fallback(index, timeout, retries, fallback_policy)`

**Parameters**:
- `index`: Fixed node index (0-based, in `node {}` declaration order)
- `timeout`: Retry interval (supports sub-second, e.g. `500ms`; below 2s clamped to 2s with WARN log, prevents probe storm)
- `retries`: Max retry attempts
  - `retries=0`: Immediately fallback on dead, no timeout wait
  - `retries>0`: Background goroutine drives retry probes at `timeout` intervals
- `fallback_policy`: Fallback selection (`random`, `min_last_latency`, `min_moving_avg_latency`, etc.)

#### How It Works

```
                  ┌─ MustGetAlive=true  ── Return fixed node (s4) normally
                  │
  Select() ───────┤                                    Natural Traffic
                  │                                       ↓
                  └─ MustGetAlive=false ── → goto doFallback → Backup (s2/s5)
                                             │
                                             ├─ Start background goroutine
                                             │     │
                                             │     ├─ ticker every timeout ─┬─ Probe OK → deadSince=0 → traffic back to fixed
                                             │     │                         └─ Fail → retryCount++
                                             │     │                                      └─ ≥retries → permanent fallback
                                             │     │
                                             │     └─ Send NotifyCheckTcp/Udp probes
                                             │
                                             └─ Subsequent Select() → deadSince≠0 → goto doFallback
```

#### Two-Layer Architecture

| Layer | Responsibility | Sends traffic to dead node? |
|-------|---------------|---------------------------|
| **Natural Traffic** (`Select()`) | Fallback immediately on dead; switch back immediately on alive | ❌ Never (first byte goes to backup) |
| **Background Goroutine** | Independent ticker-driven retry cycle; probes fixed node for recovery | ✅ Probe-only (health check requests) |

#### Four Key Event Flows

**① Node Go Dead**
```
Health check fails → Alive=false
  ├─ [Path A] aliveTransitionCallback → CompareAndSwap(false→true) → start goroutine
  └─ [Path B] Select() finds MustGetAlive=false
       └─ deadSince==0 → record timestamp → start goroutine if not running → goto doFallback
```

**② Natural Traffic During Death**
```
Select() → MustGetAlive=false → deadSince≠0 → goto doFallback → return s2/s5
```
Zero traffic wasted on dead node. **Natural traffic does NOT participate in retry counting.**

**③ Background Retry Probing**
```
Goroutine ticker fires:
  └─ MustGetAlive? ─┬─ true  → deadSince=0 → return (goroutine exits)
                      └─ false → retryCount++
                           ├─ retryCount < retries → wait next tick
                           └─ retryCount ≥ retries → permanent fallback → return
```
`NotifyCheckTcp()` and `NotifyCheckDnsUdp()` are fired each tick.

**④ Node Revive**
```
Health check or goroutine probe detects recovery → MustGetAlive=true
  ├─ Goroutine path → deadSince=0, retryCount=0 → return
  └─ Select() path → deadSince=0, retryCount=0 → return fixed node
```
**Zero-latency switch back**: recovery happens within the health check cycle (e.g. `check_interval=10s`), independent of goroutine ticker.

#### Key Implementation Details

- **aliveTransitionCallback**: Health check (`connectivity_check`) on TCP dead transition automatically starts the goroutine — no traffic required
- **deadSince Timestamp**: `fixedFallbackDeadSince` records when death was first observed, distinguishing first vs subsequent Select()
- **CompareAndSwap**: Ensures exactly one goroutine runs; `fixedFallbackStopCh` signals goroutine exit
- **Probe Cooldown**: timeout or check_interval below 2s is clamped to 2s with a WARN log, prevents probe storm
- **State Cleanup on Revive**: `MustGetAlive=true` clears both `deadSince` and `retryCount`
- **UDP-only Support**: Not limited to TCP health checks — UDP/DNS-UDP/Data-UDP dead transitions also trigger the goroutine

</details>

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
