# CFST - Cloudflare Route Quality Probe & SpeedTest

> v2.1.0 | Go Edition

CFST 已从一次性 Cloudflare IP 测速工具全面重构升级为 **GOWAY 线路质量长期探针 + 稳定性分析 + 高峰期选路数据源 (Route Quality Probe & Stability Analyzer)**。

核心设计哲学：**稳定性与保底速度 (P10) > 峰值瞬时速度**。杜绝瞬时抽水型峰值节点影响排名，为流媒体、高速隧道与科学选路提供最具韧性的前置线路决策依据。

作为测量层无缝对接 **GoPass 控制器** 与 **GOWAY 隧道**。

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
|                             +---> [1s Interval Sampling]       |
|                             |     [Anti-Buffering Floor: P10]  |
|                             |     [Stall & Zero-Speed Detect]  |
|                             |     [EWMA & History (5m~24h)]    |
|                             |     [24h Peak Hour Matrix (0~23)]|
|                             |     [5-State Debounced Health]   |
|                             |     [Multi-Horizon Scoring Engine]
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

- **CFST 职责**：**只负责测量**。执行真 1 秒粒度分片采样、卡顿率统计、计算健康度状态机与多时域综合评分，通过本地 REST API 输出结构化 `RouteMetrics`。
- **GoPass 职责**：读取 RouteMetrics，根据策略执行最优线路选择与动态故障切换。
- **GOWAY 职责**：底层纯粹的高性能通信与转发隧道。

---

## 核心特性 (v2.1.0)

- ⏱️ **真 1 秒分片采样与真实 MinSpeed** — 测速过程中每秒记录离散采样数据（`SpeedIntervalSample`），**绝不丢弃 0 MB/s 区间**；若发生断流卡顿，真实反映 `MinSpeed = 0`，消除传统测速器将最低速虚高成非零值的致命缺陷。
- 🛡️ **抗缓冲地板速度 (P10 / P25 / Median)** — 专门针对 4K/8K 视频和直播场景，采用 10 分位数速度（P10）作为防缓冲地板基准，即使峰值达到 100 MB/s，若 P10 低至 2 MB/s 也将被重罚。
- ⚠️ **卡顿检测 (Stall Detection)** — 显式追踪断流次数（`StallCount`）、累计停滞时长（`TotalStallDuration`）、最长单次停滞时长（`LongestStallDuration`）以及断流占比（`StallRate`）。
- 📊 **复合稳定性指数 (Composite Stability Index)** —
  $$\text{Stability} = 0.35 \times \text{SpeedConsistency} + 0.25 \times \text{FloorStability} + 0.20 \times \text{NoStallRatio} + 0.10 \times \text{LatencyConsistency} + 0.10 \times \text{LossConsistency}$$
- 🌙 **多时域评分引擎 (Multi-Horizon Scoring)** —
  - 正常时段：`FinalScore = Instant*0.20 + ShortTerm*0.30 + LongTerm*0.25 + PeakHour*0.15 + Conf*0.10`
  - 晚高峰时段：`FinalScore = Instant*0.10 + ShortTerm*0.25 + LongTerm*0.25 + PeakHour*0.25 + Conf*0.15`
  - 确保平稳稳定的 55 MB/s 节点得分显著高于偶发抽水 100 MB/s 且伴随断流的节点。
- 🔄 **防抖去抖健康状态机 (5-State Debounced Health)** —
  严格支持 `HEALTHY` ➔ `DEGRADED` ➔ `FAILING` ➔ `FAILED` ➔ `RECOVERING`。采用连续 3 次判定防抖机制，杜绝偶发网络波动导致频繁翻转。
- 🧭 **智能选路建议体系 (Recommendations)** — 输出 `BEST`、`GOOD`、`USABLE`、`DEGRADED`、`AVOID`、`FAILED` 等级并附带诊断原因列表。
- 📉 **EWMA 故障压制与衰减** — 线路发生探测失败时，EWMA 立即施加失败压力（`RecordFailure`），指数级衰减历史速度，防止离线节点残留虚高评分。
- 💾 **原子化安全持久化** — 支持 Windows 文件系统安全的临时文件写入、`.bak` 轮转备份与双重恢复机制。

---

## GoPass 对接 REST API

CFST 默认监听 `127.0.0.1:9876`，提供以下高可靠接口供 GoPass 控制器调用：

| 接口 | 方法 | 说明 |
|---|---|---|
| `/api/health` | GET | 获取探针状态、运行时间、版本号、各层级健康统计 |
| `/api/routes` | GET | 获取所有受控线路的 `RouteMetrics`（支持 `?tier=`, `?health=`, `?colo=`, `?limit=` 过滤） |
| `/api/routes/best` | GET | 获取经过健康过滤、高峰期修正与置信度平滑后的 Top 线路列表 |
| `/api/routes/metrics` | GET | 极轻量指标摘要数组（供 GoPass 极速高频秒级轮询） |
| `/api/routes/{ip}` | GET | 获取指定 IP 的详细信息与评分拆解 |
| `/api/routes/{ip}/history` | GET | 获取指定 IP 的 5m / 15m / 1h / 6h / 24h 滑动窗口统计 |
| `/api/routes/{ip}/peak` | GET | 获取指定 IP 的 24 小时 Peak Hour 矩阵与每小时表现 |
| `/api/routes/{ip}/samples` | GET | 获取指定 IP 最近采样的历史原始样本列表 |
| `/api/routes/tier` | POST | GoPass 通知 CFST 更新线路层级（`{"ip": "x.x.x.x", "tier": "ACTIVE"}`） |
| `/api/probe` | POST | 触发指定 IP 的即时按需探测（`{"ip": "x.x.x.x", "speed_test": true}`） |
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
  "recommendation": "BEST",
  "recommendation_reasons": [
    "Rock-solid throughput without stalls",
    "High floor speed: P10 52.8 MB/s",
    "Zero packet loss & minimal jitter"
  ],
  "stability_grade": "STABLE",
  "speed": {
    "avg": 65.2,
    "median": 64.0,
    "p10": 52.8,
    "min": 48.0
  },
  "stalls": {
    "count": 0,
    "rate": 0.0,
    "total_duration": 0.0
  },
  "rtt": 42.5,
  "jitter": 2.1,
  "packet_loss": 0.0,
  "stability": 95.4,
  "instant_score": 93.2,
  "short_term_score": 91.0,
  "long_term_score": 88.6,
  "peak_hour_score": 89.2,
  "confidence": 92.5,
  "final_score": 91.3,
  "consecutive_failures": 0,
  "consecutive_successes": 15,
  "last_tested": "2026-09-08T09:40:00+08:00"
}
```

---

## 动态综合评分权重分布

| 指标 | Normal 模式 | Peak 模式 (晚高峰) | 设计意图 |
|---|---|---|---|
| **单流速度 (Speed)** | 20% | 15% | 单连接吞吐，设定 15 MB/s (~120 Mbps) 饱和上限 |
| **抗缓冲地板速度 (P10)** | 20% | 25% | **核心保底项**，设定 12 MB/s 上限，权重与峰速等同或更高 |
| **真实最低速度 (MinSpeed)** | 10% | 10% | 断流即为 0 分，杜绝卡顿节点 |
| **复合稳定性 (Stability)** | 20% | 25% | 结合变异系数、卡顿率与时延波动的综合韧性 |
| **丢包率 (PacketLoss)** | 10% | 10% | 阶梯惩罚，严重丢包时急速归零 |
| **延迟抖动 (Jitter)** | 10% | 10% | 评估队列堆积与缓冲膨胀 |
| **TCP 延迟 (RTT)** | 5% | 2.5% | 基础往返时延 |
| **WSS 握手 (Handshake)** | 5% | 2.5% | GOWAY 协议兼容性 |

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
