# CFST
# CloudflareSpeedTest v3.3 增强版

<div align="center">

[![Version](https://img.shields.io/badge/version-3.3--enhanced-blue.svg)](https://github.com/yourusername/cfst)
[![Python](https://img.shields.io/badge/python-3.6+-green.svg)](https://www.python.org/)
[![License](https://img.shields.io/badge/license-MIT-orange.svg)](LICENSE)

一款专为 Cloudflare CDN 优化的 IP 测速工具，支持海量 IP 测试、智能采样、多次测速取平均等功能。

</div>

---

## ✨ 主要特性

- 🚀 **内存优化** - 分批处理，适合路由器等小内存设备
- 🎯 **智能采样** - 可调节采样倍率，平衡速度与覆盖率
- 📊 **多次测速** - 支持每个IP测试1-5次取平均值
- 💾 **结果保存** - 支持CSV/JSON格式导出
- 🏆 **实时排行** - 测试过程中实时显示最佳IP
- 🔄 **中断保护** - Ctrl+C中断时自动保存已测结果
- ⚡ **高性能** - 支持高并发（最高1000线程）
- 🌐 **双协议** - 支持TCP Ping和HTTP Ping模式

---

## 📦 安装依赖

### 系统要求
- Python 3.6 或更高版本
- 支持 Linux / macOS / Windows

### 安装依赖库
```bash
pip install requests tqdm urllib3
```

或使用国内镜像加速：
```bash
pip install -i https://pypi.tuna.tsinghua.edu.cn/simple requests tqdm urllib3
```

---

## 🚀 快速开始

### 1. 准备IP列表文件

创建 `ip.txt` 文件，每行一个IP或CIDR：

```text
# Cloudflare IP 范围示例
104.16.0.0/12
172.64.0.0/13
162.158.0.0/15
141.101.64.0/18
108.162.192.0/18

# 或单个IP
1.1.1.1
1.0.0.1
```

### 2. 基础测试

```bash
# 默认测试（智能采样，最多测试50000个IP）
python3 cfst_v3.3.py

# 指定IP列表文件
python3 cfst_v3.3.py -f myips.txt

# 保存结果到CSV
python3 cfst_v3.3.py -o result.csv
```

### 3. 查看结果

测试完成后会显示：
```
============================================================
📊 最终测试结果
============================================================
IP地址                   延迟      丢包率         平均速度
---------------- -------- -------- ------------
172.64.1.1        102.24ms    0.0%      19.27MB/s [19.1,19.5,19.2]
104.16.2.2        105.40ms    0.0%      18.50MB/s
162.158.3.3       108.15ms    0.0%      17.80MB/s
============================================================

🏆 推荐IP: 172.64.1.1
   延迟: 102.24ms | 平均速度: 19.27MB/s
```

---

## 📖 使用指南

### 基础参数

| 参数 | 说明 | 默认值 | 示例 |
|------|------|--------|------|
| `-n` | 并发线程数 | 200 | `-n 100` |
| `-t` | Ping测试次数 | 3 | `-t 5` |
| `-dn` | 下载测试IP数量 | 5 | `-dn 10` |
| `-dt` | 每IP下载时长(秒) | 10 | `-dt 20` |
| `-dc` | 每IP下载次数(取平均) | 1 | `-dc 3` |
| `-f` | IP列表文件路径 | ip.txt | `-f myips.txt` |
| `-o` | 保存结果文件 | - | `-o result.csv` |

### 高级参数

| 参数 | 说明 | 默认值 | 示例 |
|------|------|--------|------|
| `--max-ips` | 最多测试IP数 | 50000 | `--max-ips 10000` |
| `--sample-rate` | 采样倍率 | 1.0 | `--sample-rate 1.5` |
| `--batch-size` | 批处理大小 | 5000 | `--batch-size 3000` |
| `-allip` | 测试所有IP（慎用） | - | `-allip` |
| `-dd` | 禁用下载测试 | - | `-dd` |
| `--debug` | 显示调试信息 | - | `--debug` |

### 过滤参数

| 参数 | 说明 | 默认值 | 示例 |
|------|------|--------|------|
| `-tl` | 最大延迟(ms) | 9999 | `-tl 200` |
| `-tll` | 最小延迟(ms) | 0 | `-tll 50` |
| `-tlr` | 最大丢包率 | 0.0 | `-tlr 0.1` |
| `-sl` | 最小下载速度(MB/s) | 0 | `-sl 10` |

### HTTP Ping 参数

| 参数 | 说明 | 默认值 | 示例 |
|------|------|--------|------|
| `-httping` | 启用HTTP Ping | - | `-httping` |
| `-httping-code` | 有效状态码 | 200,301,302 | `-httping-code 200` |
| `-cfcolo` | 指定地区代码 | - | `-cfcolo LAX` |

---

## 💡 使用场景

### 场景1：快速测试（路由器推荐）

**适用于：** 小内存设备、快速筛选

```bash
python3 cfst_v3.3.py -n 100 --max-ips 5000 -dn 5 -o result.csv
```

- 并发100线程
- 最多测试5000个IP
- 只测试最佳5个IP的下载速度
- 耗时约3-5分钟

### 场景2：标准测试（推荐）

**适用于：** 日常使用、一般服务器

```bash
python3 cfst_v3.3.py -n 200 --sample-rate 1.5 --max-ips 15000 -dn 10 -dc 2 -o result.csv
```

- 并发200线程
- 1.5倍采样率（更全面）
- 最多测试15000个IP
- 测试10个最佳IP的下载速度
- 每个IP测2次取平均
- 耗时约10-15分钟

### 场景3：深度测试

**适用于：** 寻找最优IP、性能服务器

```bash
python3 cfst_v3.3.py -n 300 --sample-rate 2.0 --max-ips 30000 -dn 20 -dc 3 -o result.json
```

- 并发300线程
- 2倍采样率（覆盖更广）
- 最多测试30000个IP
- 测试20个最佳IP的下载速度
- 每个IP测3次取平均
- 耗时约30-60分钟

### 场景4：只测延迟

**适用于：** 快速筛选、延迟敏感场景

```bash
python3 cfst_v3.3.py -n 200 --max-ips 10000 -dd -o latency.csv
```

- 只测试Ping延迟
- 不进行下载测试
- 耗时约2-3分钟

### 场景5：高速网络测试

**适用于：** 千兆网络、高性能场景

```bash
python3 cfst_v3.3.py -dt 20 -dn 10 -dc 3 -url "https://speed.cloudflare.com/__down?bytes=2000000000"
```

- 每IP下载20秒
- 使用2GB测试源
- 测3次取平均
- 适合100MB/s+网络

---

## 📊 采样策略说明

### 智能采样模式（默认）

不使用 `-allip` 参数时启用，根据网段大小自动调整：

| CIDR范围 | 总IP数 | 默认采样 | 1.5倍采样 | 2.0倍采样 |
|---------|--------|---------|----------|----------|
| /12 | 104万+ | 512个 | 768个 | 1024个 |
| /16 | 6.5万 | 512个 | 768个 | 1024个 |
| /17-/20 | 4096-32768 | 50个 | 75个 | 100个 |
| /21-/24 | 128-2048 | 10个 | 15个 | 20个 |
| /25-/32 | 1-128 | 1-3个 | 2-5个 | 3-6个 |

### 全部测试模式（-allip）

使用 `-allip` 参数时：
- 小网段（<1000 IP）：测试全部
- 大网段（>1000 IP）：随机采样1000个（可用 `--sample-rate` 调整）
- 受 `--max-ips` 全局限制

**警告：** 不建议对大型网段使用 `-allip`，会导致内存溢出！

---

## 📁 结果文件格式

### CSV 格式

```csv
IP地址,延迟(ms),丢包率(%),平均速度(MB/s),测试次数,各次速度(MB/s),测试时间
172.64.1.1,102.24,0.0,19.27,3,"19.1,19.5,19.2",2025-01-08 22:30:15
104.16.2.2,105.40,0.0,18.50,1,"18.5",2025-01-08 22:30:45
```

**导入Excel：**
1. 打开Excel
2. 数据 → 从文本/CSV
3. 选择UTF-8编码
4. 自动识别分隔符

### JSON 格式

```json
{
  "test_info": {
    "version": "v3.3-enhanced",
    "test_time": "2025-01-08 22:30:00",
    "total_ips_tested": 5234,
    "sample_rate": 1.5,
    "download_count_per_ip": 3
  },
  "results": [
    {
      "ip": "172.64.1.1",
      "latency_ms": 102.24,
      "loss_rate": 0.0,
      "avg_speed_mbps": 19.27,
      "all_speeds_mbps": [19.1, 19.5, 19.2],
      "test_time": "2025-01-08 22:30:15"
    }
  ]
}
```

---

## 🔧 常见问题

### Q1: 程序被 Killed / 内存不足

**原因：** IP数量过多，内存溢出

**解决方案：**
```bash
# 减少最大IP数
python3 cfst_v3.3.py --max-ips 5000

# 减少批处理大小
python3 cfst_v3.3.py --batch-size 2000

# 减少并发线程
python3 cfst_v3.3.py -n 50

# 降低采样倍率
python3 cfst_v3.3.py --sample-rate 0.5
```

### Q2: 下载速度显示0.00MB/s

**原因：** 连接失败或超时

**解决方案：**
```bash
# 使用调试模式查看详细错误
python3 cfst_v3.3.py --debug

# 增加下载时长
python3 cfst_v3.3.py -dt 20

# 使用HTTP Ping模式
python3 cfst_v3.3.py -httping
```

### Q3: 测试太慢

**解决方案：**
```bash
# 减少测试IP数量
python3 cfst_v3.3.py --max-ips 3000 -dn 3

# 降低采样倍率
python3 cfst_v3.3.py --sample-rate 0.5

# 只测延迟，跳过下载
python3 cfst_v3.3.py -dd

# 减少每IP测试次数
python3 cfst_v3.3.py -dc 1 -dt 5
```

### Q4: 想测试更多IP

**解决方案：**
```bash
# 增加采样倍率
python3 cfst_v3.3.py --sample-rate 2.0 --max-ips 50000

# 分段测试
python3 cfst_v3.3.py -f ip_part1.txt -o result1.csv
python3 cfst_v3.3.py -f ip_part2.txt -o result2.csv
```

### Q5: 如何获取 Cloudflare IP 列表？

官方IP范围：
```bash
# IPv4
curl https://www.cloudflare.com/ips-v4 > ip.txt

# IPv6
curl https://www.cloudflare.com/ips-v6 > ipv6.txt
```

或手动访问：https://www.cloudflare.com/zh-cn/ips/

---

## 🎯 性能优化建议

### 路由器/小内存设备
```bash
python3 cfst_v3.3.py \
  -n 50 \
  --max-ips 3000 \
  --batch-size 1000 \
  -dn 3 \
  -dc 1 \
  -o result.csv
```

### 一般Linux服务器
```bash
python3 cfst_v3.3.py \
  -n 200 \
  --sample-rate 1.5 \
  --max-ips 20000 \
  -dn 10 \
  -dc 2 \
  -o result.csv
```

### 高性能服务器
```bash
python3 cfst_v3.3.py \
  -n 500 \
  --sample-rate 3.0 \
  --max-ips 100000 \
  --batch-size 10000 \
  -dn 20 \
  -dc 3 \
  -o result.json
```

---

## 📈 版本历史

### v3.3-enhanced (2025-01-08)
- ✨ 新增结果保存功能（CSV/JSON）
- ✨ 新增智能采样倍率调整
- ✨ 新增多次下载测速取平均
- ✨ 新增实时最佳IP显示
- 🐛 修复中断保护机制
- 🚀 优化内存占用

### v3.2-memory-optimized
- 🚀 内存优化，支持大规模IP测试
- 🔄 分批处理机制
- 📊 生成器模式减少内存占用

### v3.1-fixed
- 🐛 修复下载测试失败问题
- ✨ 新增原始Socket下载方法
- 🔧 改进调试模式

### v3.0-optimized
- 🎉 首个优化版本
- 🚀 大幅性能提升
- 📝 代码重构

---

## 🤝 贡献指南

欢迎提交Issue和Pull Request！

### 开发环境
```bash
git clone https://github.com/yourusername/cfst.git
cd cfst
pip install -r requirements.txt
```

### 运行测试
```bash
python3 cfst_v3.3.py --debug -n 10 --max-ips 100
```

---

## 📄 许可证

MIT License - 详见 [LICENSE](LICENSE) 文件

---

## 🙏 致谢

- Cloudflare 提供的测速服务
- 所有贡献者和使用者的反馈

---

## 📧 联系方式

- Issues: [GitHub Issues](https://github.com/yourusername/cfst/issues)
- Email: your.email@example.com

---

<div align="center">

**如果这个项目对你有帮助，请给个 ⭐ Star！**

Made with ❤️ by [Your Name]

</div>
