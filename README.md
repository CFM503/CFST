# CFST - Cloudflare & YouTube CDN SpeedTest

> v1.8.5 | Go Edition

从 Cloudflare 或 YouTube CDN 节点中筛选出**高速、低延迟、高稳定性**的 IP，专为流媒体场景优化。

## 功能特性

- **Cloudflare 测速** — 扫描 Cloudflare IP 段，筛选最优节点
- **YouTube CDN 测速** — 解析 googlevideo.com 节点，直测 YouTube CDN
- **Colo 优先策略** — 批量检测数据中心，自动筛选最优 Colo 节点组
- **并行测速** — 多个 IP 同时测速，大幅提高筛选效率
- **代理支持** — SOCKS5 / HTTP 代理，解决 DNS 劫持和网络限制
- **抖动检测** — 5 次 ping 测量延迟标准差，评估连接稳定性
- **多点采样测速** — 每 2 秒记录瞬时速度，计算速度稳定性
- **流媒体评分公式** — 单流速度 35% + 最低速度 20% + 稳定性 25% + 延迟 10% + 抖动 10%
- **Web UI** — 浏览器可视化测试，实时 SSE 推送进度
- **CSV 导出** — 结果可导出为 CSV 文件

## 快速开始

### 基本用法（Cloudflare）

```bash
# 默认扫描 5000 个 IP，Colo 筛选后并行测速
cfst.exe

# 自定义参数
cfst.exe -max 10000 -topn 200 -dlc 5 -dn 20

# 快速模式（少量 IP，快速出结果）
cfst.exe -max 1000 -topn 30 -dlc 3 -dt 10
```

### YouTube CDN 测速

```bash
# 测试 YouTube CDN 节点（需要代理）
cfst.exe -yt -proxy socks5://127.0.0.1:1080

# 限制扫描数量
cfst.exe -yt -max 100 -proxy 127.0.0.1:1080
```

### Web UI 模式

```bash
# 启动 Web 服务
cfst.exe -web

# 浏览器访问 http://localhost:9876
# 支持在页面中配置代理和 YouTube 模式
```

### 自定义 URL 测速

```bash
# 使用自定义下载地址测速（绕过 speed.cloudflare.com 限速）
cfst.exe -url "https://example.com/test100m.bin" -dn 20 -dt 20
```

## 参数说明

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-p` | 443 | 目标端口 |
| `-max` | 5000 | 最大扫描 IP 数 |
| `-topn` | 100 | 延迟最低的前 N 个候选进入测速 |
| `-dlc` | 3 | 并行下载测试并发数 |
| `-dn` | 10 | 下载测试数量 |
| `-dt` | 15 | 下载测试时长（秒） |
| `-st` | 15.0 | 停止阈值（MB/s） |
| `-u` | false | C 段去重 |
| `-f` | - | 自定义 IP 文件 |
| `-o` | result_colo.csv | 输出文件 |
| `-sc` | 200 | 扫描并发数 |
| `-skip429` | true | 静默丢弃 429 节点 |
| `-url` | CF 测速 URL | 自定义下载测试 URL |
| `-yt` | false | YouTube CDN 测试模式 |
| `-proxy` | - | 代理地址（socks5://ip:port 或 http://ip:port） |
| `-web` | false | 启动 Web UI |
| `-web <port>` | 9876 | Web UI 端口 |

## 输出指标

| 指标 | 说明 |
|------|------|
| **IP** | 节点 IP 地址 |
| **Colo** | 数据中心代号 |
| **Latency** | TCP 延迟（5 次平均） |
| **Jitter** | 延迟抖动（标准差） |
| **SgSpeed** | 单流下载速度（MB/s） — 最贴近真实体验 |
| **Speed** | 多线程聚合下载速度（MB/s） |
| **MinSpeed** | 最低瞬时速度 |
| **LoadLatency** | 负载延迟（ms） — 带宽满载时的 TCP 延迟 |
| **Stability** | 速度稳定性（0-100%） |
| **Score** | 综合评分 |

## 评分公式

```
Score = 速度×35% + 最低速度×20% + 延迟×10% + 抖动×10% + 稳定性×25% + Colo奖励
```

- **速度 35%** — 单流下载速度，cap 15 MB/s（120 Mbps，足够 4K）
- **最低速度 20%** — 决定缓冲风险，cap 10 MB/s（80 Mbps）
- **稳定性 25%** — 避免缓冲卡顿，变异系数越小越高
- **延迟 10%** — 影响起播速度和清晰度切换
- **抖动 10%** — 影响播放流畅度，>10ms 开始扣分

## 编译

```bash
# Windows
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o cfst.exe .

# Linux
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o cfst .

# macOS
GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w" -o cfst .
```

## 代理配置

推荐使用 SOCKS5 代理，DNS 查询也会走代理，解决 DNS 劫持问题：

```bash
# SOCKS5（推荐）
cfst.exe -yt -proxy socks5://127.0.0.1:1080

# 简写（默认 SOCKS5）
cfst.exe -yt -proxy 127.0.0.1:1080

# HTTP 代理
cfst.exe -yt -proxy http://127.0.0.1:8080
```

## 项目结构

```
cfst-go/
├── main.go       # 入口、参数解析
├── engine.go     # 核心引擎：IP生成、TCP Ping、HTTP客户端、测速
├── scanner.go    # 扫描流程：Config、ScanPing、DetectColo、RunDownloadTest、RunCLI
├── web.go        # Web UI 服务端
└── index.html    # Web UI 前端页面
```

## License

MIT
