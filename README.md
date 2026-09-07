# CFST - Cloudflare Route Quality Probe & SpeedTest

> v2.0.1 | Go Edition

CFST 已从一次性 Cloudflare IP 测速工具全面升级为**长期运行的线路质量检测器 (Route Quality Probe)**。

专为流媒体、高速连接与科学选路场景打造，作为测量层无缝对接 **GoPass 控制器** 与 **GOWAY 隧道**。

---

## 整体架构与职责分工

```
+----------------------------------------------------------------+
|                     CFST (Route Quality Probe)                 |
|                      【职责：测量层 (Measure)】                  |
|                                                                |
|  [Initial Scan]  ---> [Candidate Pool]                         |
|                             |                                  |
|  [Background Probe]  <------+                                  |
|   - Active (高频)           |                                  |
|   - Standby (中频)          +---> [Layered Probe: L1~L5]       |
|   - Candidate (低频)        |          |                       |
|   - Failed (恢复探测)        |          v                       |
|                             +---> [EWMA & History (5m~24h)]    |
|                             |     [Peak Hour Matrix (00~23)]   |
|                             |     [Health State Machine]       |
|                             |     [Dynamic Score: Inst/ST/LT]  |
+-----------------------------+----------------------------------+
                              |
                     RouteMetrics (JSON API)
                              |
                              v
+----------------------------------------------------------------+
|                        GoPass Controller                       |
|                      【职责：控制与决策层】                      |
+----------------------------------------------------------------+
                              |
                              v
+----------------------------------------------------------------+
|                             GOWAY                              |
|                      【职责：高速隧道层】                        |
+----------------------------------------------------------------+
```

- **CFST 职责**：**只负责测量**。探测网络状态，计算健康度与评分，通过本地 REST API 输出统一 `RouteMetrics`。
- **GoPass 职责**：读取 RouteMetrics，执行策略判定与线路切换控制。
- **GOWAY 职责**：底层纯粹的高性能通信与转发隧道。

---

## 核心特性

- 🎯 **统一 RouteMetrics** — 结构化输出 IP、Port、Colo、RTT、丢包率、抖动、单流速度、最低速度、稳定性、负载延迟、握手状态、健康状态及三维评分。
- 🕒 **解决“瞬时高速节点”** — 创新采用三维时域评分：
  - `InstantScore`：当前测量得分（占比 40%）
  - `ShortTermScore`：最近 5~15 分钟 EWMA 得分（占比 35%）
  - `LongTermScore`：数小时长期表现及晚高峰加权得分（占比 25%）
- 📈 **EWMA 指数加权移动平均** — 对速度、延迟、丢包率、抖动、稳定性进行双半衰期平滑，避免瞬时波动干扰选路。
- 🌙 **24小时 Peak Hour 矩阵** — 实时统计 00:00~23:00 每小时的表现与失败率，自动学习“晚高峰掉速节点”并动态调整评分。
- 🛡️ **退化与健康状态机 (Route Health)** — 严格区分单次测量成功与线路健康，支持 5 态生命周期：
  `HEALTHY` ➔ `DEGRADED` ➔ `FAILING` ➔ `FAILED` ➔ `RECOVERING`。
- ⚡ **分层主动探测 (Layered Probe Pipeline)** —
  - Layer 1: TCP Ping（延迟与丢包）
  - Layer 2: WSS Handshake（GOWAY 兼容性握手）
  - Layer 3: Quick Speed（2~3秒快速测速）
  - Layer 4: Full Speed Test（深度吞吐测试）
  - Layer 5: Load Latency（负载延迟测试）
  由浅入深逐层过滤，严格限制带宽并发（默认最多 1 个测速任务并发），防止探针影响真实用户流量。
- 🔄 **分级探测调度 (Tiered Scheduler)** —
  - `Active` 线路：高频轻量保活与质量监测（默认 10s）
  - `Standby` 备用线路：中频探活（默认 30s）
  - `Candidate` 候选池：低频轮询（默认 180s）
  - `Failed` 故障线路：恢复探测（默认 60s）
- 🔌 **GoPass 专用本地 REST API** — 监听 `127.0.0.1:9876`，提供结构化 JSON 数据，无须解析文本或 CSV。
- 📦 **100% 兼容现有工具生态** — 保留原有 CLI 交互模式、Web UI 界面与 `result_colo.csv` 输出。

---

## 快速开始

### 1. 长期探针模式 (Daemon Mode，推荐用于 GoPass 对接)

```bash
# 启动后台常驻线路质量检测器与本地 API
cfst.exe -daemon

# 自定义探针频率与监听地址
cfst.exe -daemon -api-addr 127.0.0.1:9876 -active-interval 10 -standby-interval 30 -mode peak
```

### 2. 传统一次性测速模式 (CLI)

```bash
# 默认扫描 3000 个 IP，筛选后测速并生成 result_colo.csv
cfst.exe

# 自定义参数扫描
cfst.exe -max 5000 -topn 100 -dlc 3 -dn 20
```

### 3. Web UI 浏览器可视化模式

```bash
# 启动 Web 服务
cfst.exe -web

# 浏览器访问 http://127.0.0.1:9876
```

---

## GoPass 对接 REST API

CFST 在后台运行时默认监听 `127.0.0.1:9876`，提供以下接口供 GoPass 控制器调用：

| 接口 | 方法 | 说明 |
|---|---|---|
| `/api/health` | GET | 获取探针运行状态、运行时间、线路健康数统计 |
| `/api/routes` | GET | 获取所有受控线路的 `RouteMetrics`（支持 `?tier=`, `?health=`, `?colo=` 过滤） |
| `/api/routes/best` | GET | 获取综合得分最高且健康的最优线路（支持 `?limit=N`，默认前 5） |
| `/api/routes/metrics` | GET | 轻量级指标摘要数组（适配 GoPass 极速高频轮询） |
| `/api/routes/{ip}` | GET | 获取指定 IP 的详细时域滑动窗口 (5m~24h)、EWMA 与 Peak Hour 矩阵 |
| `/api/routes/tier` | POST | GoPass 通知 CFST 更新线路层级（`{"ip": "x.x.x.x", "tier": "ACTIVE"}`） |
| `/api/probe` | POST | 触发指定 IP 的即时探测（`{"ip": "x.x.x.x", "speed_test": true}`） |
| `/api/config` | GET / POST | 查看或动态切换运行模式（normal/peak）与探针间隔 |

### 统一输出结构 RouteMetrics 示例

```json
{
  "id": "route-162-159-192-1-443",
  "ip": "162.159.192.1",
  "port": 443,
  "colo": "HKG",
  "tier": "ACTIVE",
  "health": "HEALTHY",
  "rtt": 42.5,
  "packet_loss": 0.0,
  "jitter": 2.1,
  "download_speed": 68.4,
  "single_speed": 65.2,
  "min_speed": 52.8,
  "stability": 95.4,
  "load_latency": 55.2,
  "handshake_success": true,
  "instant_score": 93.2,
  "short_term_score": 91.0,
  "long_term_score": 88.6,
  "final_score": 91.3,
  "ewma": {
    "speed": 64.8,
    "latency": 43.1,
    "loss": 0.0,
    "jitter": 2.3,
    "stability": 94.8
  },
  "consecutive_failures": 0,
  "consecutive_successes": 15,
  "last_tested": "2026-09-07T22:45:00+08:00",
  "timestamp": "2026-09-07T22:45:00+08:00"
}
```

---

## 动态综合评分公式

$$\text{FinalScore} = \text{InstantScore} \times 0.40 + \text{ShortTermScore} \times 0.35 + \text{LongTermScore} \times 0.25$$

### 维度权重分布（支持 API 动态调整）

| 指标 | Normal 模式 | Peak 模式 (晚高峰) | 说明 |
|---|---|---|---|
| **单流速度 (SingleSpeed)** | 25% | 15% | 单连接下载带宽（cap 15 MB/s） |
| **保底速度 (MinSpeed)** | 10% | 10% | 瞬时最低速度，保障视频不出现断流缓冲 |
| **稳定性 (Stability)** | 25% | 30% | 速度变异系数，数值越高速度越平稳 |
| **丢包率 (PacketLoss)** | 15% | 25% | 丢包严重度惩罚（20% 丢包即得 0 分） |
| **延迟抖动 (Jitter)** | 10% | 10% | RTT 方差标准差（>10ms 阶梯扣分） |
| **TCP 延迟 (RTT)** | 10% | 5% | 基础往返时延 |
| **WSS 握手 (Handshake)** | 5% | 5% | GOWAY 兼容性判定 |
| **Colo 奖励** | +5.0 | +3.0 | 优质数据中心直接加分 |

---

## 命令行参数一览

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-daemon` | false | 启动常驻 Route Quality Probe 探针模式 |
| `-api-addr` | 127.0.0.1:9876 | REST API 本地监听地址 |
| `-mode` | normal | 评分模式（`normal` 或 `peak`） |
| `-active-interval` | 10 | Active 线路探测周期（秒） |
| `-standby-interval` | 30 | Standby 线路探测周期（秒） |
| `-candidate-interval` | 180 | Candidate 候选池探测周期（秒） |
| `-failed-interval` | 60 | Failed 故障线路复活重试周期（秒） |
| `-state` | cfst_state.json | 线路状态与时序历史持久化文件 |
| `-p` | 443 | 目标端口 |
| `-max` | 3000 | 初始扫描最大 IP 数量 |
| `-topn` | 100 | 初筛进入详细测试的候选数量 |
| `-dlc` | 1 | 测速并发数 |
| `-dn` | 20 | 测速节点数量 |
| `-dt` | 20 | 测速持续时长（秒） |
| `-st` | 30.0 | 停止阈值（MB/s） |
| `-u` | false | C 段去重 |
| `-f` | - | 自定义 IP 列表文件 |
| `-o` | result_colo.csv | CSV 结果输出路径 |
| `-sc` | 200 | TCP 扫描并发度 |
| `-skip429` | true | 静默跳过 429 限流节点 |
| `-url` | speed.cloudflare.com | 自定义下载测速 URL |
| `-sni` | - | 自定义 TLS SNI |
| `-wsshost` | colo.4467107.xyz | GOWAY WSS 握手校验 Fake Host |
| `-web` | false | 启动 Web UI 界面 |

---

## 编译与发布

```bash
# Windows x64
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o cfst_windows_amd64.exe ./cfst-go

# Linux x64
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o cfst_linux_amd64 ./cfst-go

# macOS arm64
GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o cfst_darwin_arm64 ./cfst-go
```

---

## 项目目录结构

```
CFST/
├── .github/workflows/
│   └── release.yml          # GitHub Actions 跨平台自动打包发布
├── README.md                # 架构设计与文档
├── .gitignore               # 忽略编译文件、持久化状态与临时数据
└── cfst-go/
    ├── main.go              # CLI 入口与参数解析
    ├── daemon.go            # Probe 守护进程生命周期与退出保存
    ├── metrics.go           # 统一 RouteMetrics 与健康状态机
    ├── score.go             # 多时域评分引擎（Instant/Short/Long/Final）
    ├── ewma.go              # 双半衰期 EWMA 指数移动平均跟踪器
    ├── history.go           # 滑动窗口 (5m~24h)、24h Peak Hour 矩阵与 RouteStore
    ├── probe.go             # 分级分层后台探针调度器 (L1~L5)
    ├── api.go               # GoPass 专用本地 REST JSON API
    ├── scanner.go           # 扫描流水线、CSV 生成与旧版兼容
    ├── engine.go            # 网络底层探测 primitives (Ping, WSS, Speed)
    ├── web.go               # Web UI 与 SSE 实时事件服务
    ├── index.html           # 前端可视化交互页面
    ├── CHANGELOG.md         # 版本发布日志
    └── *_test.go            # 单元测试与端到端测试套件
```

---

## License

MIT
