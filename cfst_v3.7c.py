#!/usr/bin/env python3
"""
CloudflareSpeedTest v3.7 - 稳定性优化版
新增功能：
- 速度稳定性分析（标准差、变异系数）
- 综合评分系统（平衡速度和稳定性）
- 详细可视化输出
- 多种排序模式
"""
import os
import sys
import time
import random
import socket
import ipaddress
import threading
import argparse
import ssl
import csv
import json
import asyncio
import math
import statistics
from abc import ABC, abstractmethod
from concurrent.futures import ThreadPoolExecutor
from typing import Optional, Tuple, List, Iterator, Dict
from dataclasses import dataclass, field
from urllib.parse import urlparse
from datetime import datetime
import warnings

# 检查依赖
try:
    from tqdm import tqdm
    import requests
    import urllib3
except ImportError as e:
    print(f"缺少必要的库: {e}")
    print("请运行: pip install requests tqdm urllib3")
    sys.exit(1)

# 检查asyncio支持
ASYNC_AVAILABLE = sys.version_info >= (3, 7)
if not ASYNC_AVAILABLE:
    print("⚠️  警告: Python 3.7+ 才支持异步功能，当前版本将禁用异步模式")

warnings.filterwarnings('ignore')
urllib3.disable_warnings()

# 常量配置
DEFAULT_PORT = 443
DEFAULT_URL = "https://speed.cloudflare.com/__down?bytes=1000000000"
DEFAULT_THREADS = 200
DEFAULT_TEST_COUNT = 3
DEFAULT_DOWNLOAD_COUNT = 10
DEFAULT_DOWNLOAD_SECONDS = 10
DEFAULT_IP_FILE = "ip.txt"
DEFAULT_TIMEOUT = 5
DEFAULT_MAX_LOSS_RATE = 0.0
DEFAULT_ASYNC_LIMIT = 512
MAX_SAFE_ASYNC_LIMIT = 2048
VERSION = "v3.7-stability"

# 采样限制
CIDR_SAMPLE_LIMIT = 1000


@dataclass
class TestResult:
    """测试结果数据类"""
    ip: str
    latency: float
    loss_rate: float
    download_speed: float = 0.0
    download_speeds: List[float] = field(default_factory=list)
    test_time: str = ""
    
    # 稳定性指标
    speed_std: float = 0.0      # 标准差
    speed_min: float = 0.0      # 最小速度
    speed_max: float = 0.0      # 最大速度
    speed_cv: float = 0.0       # 变异系数 (Coefficient of Variation)
    stability_score: float = 0.0  # 稳定性得分 (0-100)
    composite_score: float = 0.0  # 综合得分
    
    def __post_init__(self):
        if not self.test_time:
            self.test_time = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
    
    def calculate_stability_metrics(self):
        """计算稳定性指标"""
        if not self.download_speeds or len(self.download_speeds) < 2:
            return
        
        valid_speeds = [s for s in self.download_speeds if s > 0]
        if len(valid_speeds) < 2:
            return
        
        # 基础统计
        self.speed_min = min(valid_speeds)
        self.speed_max = max(valid_speeds)
        self.download_speed = statistics.mean(valid_speeds)
        
        # 标准差
        self.speed_std = statistics.stdev(valid_speeds)
        
        # 变异系数 (CV = std / mean)
        if self.download_speed > 0:
            self.speed_cv = (self.speed_std / self.download_speed) * 100
        
        # 稳定性得分 (0-100分，CV越小越稳定)
        # CV < 5%: 90-100分
        # CV 5-10%: 70-90分
        # CV 10-20%: 50-70分
        # CV > 20%: 0-50分
        if self.speed_cv < 5:
            self.stability_score = 100 - self.speed_cv
        elif self.speed_cv < 10:
            self.stability_score = 90 - (self.speed_cv - 5) * 4
        elif self.speed_cv < 20:
            self.stability_score = 70 - (self.speed_cv - 10) * 2
        else:
            self.stability_score = max(0, 50 - (self.speed_cv - 20))
        
        # 综合得分 = 平均速度 × 稳定性权重
        # 稳定性权重 = stability_score / 100
        stability_weight = self.stability_score / 100
        self.composite_score = self.download_speed * stability_weight
    
    def get_stability_stars(self) -> str:
        """获取稳定性星级"""
        if self.speed_cv < 3:
            return "⭐⭐⭐⭐⭐"
        elif self.speed_cv < 5:
            return "⭐⭐⭐⭐"
        elif self.speed_cv < 10:
            return "⭐⭐⭐"
        elif self.speed_cv < 20:
            return "⭐⭐"
        else:
            return "⭐"
    
    def get_recommendation(self) -> str:
        """获取推荐等级"""
        if self.composite_score >= 90:
            return "⭐⭐⭐⭐⭐ 非常推荐"
        elif self.composite_score >= 70:
            return "⭐⭐⭐⭐ 推荐使用"
        elif self.composite_score >= 50:
            return "⭐⭐⭐ 可以使用"
        elif self.composite_score >= 30:
            return "⭐⭐ 不太推荐"
        else:
            return "⭐ 不推荐"


class PingExecutor(ABC):
    """Ping测试执行器抽象基类"""
    
    @abstractmethod
    def execute(self, ip_list: List[str]) -> List[TestResult]:
        """执行Ping测试"""
        pass


class SyncPingExecutor(PingExecutor):
    """同步Ping执行器"""
    
    def __init__(self, tester):
        self.tester = tester
    
    def execute(self, ip_list: List[str]) -> List[TestResult]:
        """使用线程池执行同步Ping测试"""
        results = []
        results_lock = threading.Lock()
        
        def worker(ip):
            result = self.tester.tcp_ping(ip) if not self.tester.args.httping else self.tester.http_ping(ip)
            if result:
                latency, loss_rate = result
                if (self.tester.args.tll <= latency <= self.tester.args.tl and 
                    loss_rate <= self.tester.args.tlr):
                    with results_lock:
                        results.append(TestResult(ip, latency, loss_rate))
        
        with ThreadPoolExecutor(max_workers=self.tester.args.n) as executor:
            list(tqdm(
                executor.map(worker, ip_list),
                total=len(ip_list),
                desc="  ⏱️  Ping测试(同步)",
                ncols=80,
                leave=False
            ))
        
        return results


class AsyncPingExecutor(PingExecutor):
    """异步Ping执行器"""
    
    def __init__(self, tester):
        self.tester = tester
    
    async def tcp_ping_async(self, ip: str, semaphore: asyncio.Semaphore) -> Optional[Tuple[float, float]]:
        """异步TCP连接测试"""
        success_count = 0
        latencies = []
        timeout = self.tester.args.timeout
        
        for i in range(self.tester.args.t):
            try:
                async with semaphore:
                    start = time.perf_counter()
                    reader, writer = await asyncio.wait_for(
                        asyncio.open_connection(ip, self.tester.args.tp),
                        timeout=timeout
                    )
                    latency_ms = (time.perf_counter() - start) * 1000
                    
                    try:
                        writer.close()
                        await writer.wait_closed()
                    except:
                        pass
                
                success_count += 1
                latencies.append(latency_ms)
                
            except (asyncio.TimeoutError, OSError, ConnectionRefusedError):
                if i == 0:
                    return None
                continue
            finally:
                if i < self.tester.args.t - 1:
                    await asyncio.sleep(0.001)
        
        if success_count == 0:
            return None
        
        filtered_latencies = self.tester.filter_latency_samples(ip, latencies)
        avg_latency = self.tester.aggregate_latency(filtered_latencies)
        loss_rate = 1 - (success_count / self.tester.args.t)
        
        return avg_latency, loss_rate
    
    async def worker_async(self, ip: str, semaphore: asyncio.Semaphore) -> Optional[TestResult]:
        """异步工作协程"""
        result = await self.tcp_ping_async(ip, semaphore)
        
        if result:
            latency, loss_rate = result
            if (self.tester.args.tll <= latency <= self.tester.args.tl and 
                loss_rate <= self.tester.args.tlr):
                return TestResult(ip, latency, loss_rate)
        return None
    
    async def execute_async(self, ip_list: List[str]) -> List[TestResult]:
        """异步执行Ping测试"""
        semaphore = asyncio.Semaphore(self.tester.args.async_ping_limit)
        tasks = [self.worker_async(ip, semaphore) for ip in ip_list]
        
        results = []
        with tqdm(total=len(ip_list), desc="  ⏱️  Ping测试(异步)", ncols=80, leave=False) as pbar:
            for coro in asyncio.as_completed(tasks):
                result = await coro
                if result:
                    results.append(result)
                pbar.update(1)
        
        return results
    
    def execute(self, ip_list: List[str]) -> List[TestResult]:
        """同步接口"""
        return asyncio.run(self.execute_async(ip_list))


class DownloadExecutor(ABC):
    """下载测试执行器抽象基类"""
    
    @abstractmethod
    def execute(self, test_ips: List[str]) -> Dict[str, float]:
        """执行下载测试"""
        pass


class SyncSerialDownloadExecutor(DownloadExecutor):
    """同步串行下载执行器（默认，最准确）"""
    
    def __init__(self, tester):
        self.tester = tester
    
    def execute(self, test_ips: List[str]) -> Dict[str, float]:
        """单线程串行下载测试"""
        results = {}
        
        print(f"📥 下载模式: 同步串行（单线程，最准确）✅")
        print(f"⚙️  每IP测试次数: {self.tester.args.dc}次")
        print(f"⚙️  每次测试时长: {self.tester.args.dt}秒")
        print(f"⚙️  预计总耗时: {len(test_ips) * self.tester.args.dc * self.tester.args.dt}秒\n")
        
        for i, ip in enumerate(test_ips, 1):
            progress = f"[{i}/{len(test_ips)}]"
            percentage = f"({i*100//len(test_ips)}%)"
            
            print(f"{progress} {percentage} 测试 {ip}...")
            
            # 执行多次下载测试
            speeds = []
            for test_num in range(self.tester.args.dc):
                if self.tester.args.dc > 1:
                    print(f"    第 {test_num+1}/{self.tester.args.dc} 次: ", end="", flush=True)
                
                speed = self.tester.download_test_socket(ip)
                speeds.append(speed)
                
                if self.tester.args.dc > 1:
                    print(f"{speed:.2f} MB/s")
                
                if speed == 0 and test_num == 0:
                    break
                
                if test_num < self.tester.args.dc - 1 and speed > 0:
                    time.sleep(0.5)
            
            # 计算平均速度
            valid_speeds = [s for s in speeds if s > 0]
            avg_speed = sum(valid_speeds) / len(valid_speeds) if valid_speeds else 0.0
            
            results[ip] = avg_speed
            
            # 更新结果（包含所有速度数据）
            self.tester.update_result(ip, download_speeds=speeds)
            
            # 计算稳定性指标
            result = self.tester.results_dict.get(ip)
            if result:
                result.calculate_stability_metrics()
                
                # 显示汇总
                if len(valid_speeds) > 1:
                    print(f"    ✅ 平均: {result.download_speed:.2f} MB/s | "
                          f"范围: {result.speed_min:.2f}-{result.speed_max:.2f} | "
                          f"稳定性: {result.get_stability_stars()} (CV: {result.speed_cv:.1f}%)")
                elif len(valid_speeds) == 1:
                    print(f"    ✅ 速度: {avg_speed:.2f} MB/s")
                else:
                    print(f"    ❌ 下载失败")
            
            # 显示剩余时间
            if i < len(test_ips):
                remaining = (len(test_ips) - i) * self.tester.args.dc * self.tester.args.dt
                print(f"    ⏱️  剩余时间: 约{remaining}秒\n")
            else:
                print()
            
            # 实时最佳IP
            if i % 3 == 0 or i == len(test_ips):
                self.tester.update_best_ips_display()
        
        return results


class CloudflareSpeedTest:
    def __init__(self):
        self.results: List[TestResult] = []
        self.results_dict: Dict[str, TestResult] = {}
        self.lock = threading.Lock()
        self.args = self.parse_args()
        self.valid_http_codes = set(self.args.httping_code.split(","))
        self.debug = self.args.debug
        self.total_ips_tested = 0
        self.sample_multiplier = self.args.sample_rate
        self.parsed_url = urlparse(self.args.url)
        
        # 参数验证和警告
        self.validate_and_warn()
        
        # 选择执行器
        self.ping_executor = self.select_ping_executor()
        self.download_executor = self.select_download_executor()
    
    def parse_args(self) -> argparse.Namespace:
        parser = argparse.ArgumentParser(
            description=f"Cloudflare CDN IP 测速工具 {VERSION} - 稳定性优化版",
            formatter_class=argparse.RawDescriptionHelpFormatter,
            epilog="""
示例:
  完整测试（推荐）:
    python3 %(prog)s --async-ping -dc 10 -dn 10 -o result.csv
  
  快速稳定性测试:
    python3 %(prog)s --async-ping -dc 5 -dn 20 --sort stability
            """
        )
        # 基础参数
        parser.add_argument("-n", type=int, default=DEFAULT_THREADS, 
                          help="同步模式并发线程数 (默认200)")
        parser.add_argument("-t", type=int, default=DEFAULT_TEST_COUNT, 
                          help="Ping测试次数 (默认3)")
        parser.add_argument("-dn", type=int, default=DEFAULT_DOWNLOAD_COUNT, 
                          help="下载测试IP数量 (默认10)")
        parser.add_argument("-dt", type=int, default=DEFAULT_DOWNLOAD_SECONDS, 
                          help="下载测试时长/秒 (默认10)")
        parser.add_argument("-dc", type=int, default=5,
                          help="下载测试次数/取平均 (默认5，建议5-10)")
        parser.add_argument("-tp", type=int, default=DEFAULT_PORT, 
                          help="测试端口 (默认443)")
        parser.add_argument("-url", default=DEFAULT_URL, 
                          help="下载测试URL")
        parser.add_argument("--timeout", type=int, default=DEFAULT_TIMEOUT,
                          help="连接超时时间/秒 (默认5)")
        
        # 异步参数
        parser.add_argument("--async-ping", action="store_true",
                          help="使用异步IO进行Ping测试")
        parser.add_argument("--async-ping-limit", type=int, default=DEFAULT_ASYNC_LIMIT,
                          help=f"异步Ping最大并发数 (默认{DEFAULT_ASYNC_LIMIT})")
        
        # HTTP Ping参数
        parser.add_argument("-httping", action="store_true", 
                          help="使用HTTP ping")
        parser.add_argument("-httping-code", default="200,301,302", 
                          help="HTTP ping有效状态码")
        parser.add_argument("-cfcolo", default="", 
                          help="指定Cloudflare地区代码")
        
        # 过滤参数
        parser.add_argument("-tl", type=int, default=9999, 
                          help="最大延迟/毫秒")
        parser.add_argument("-tll", type=int, default=0, 
                          help="最小延迟/毫秒")
        parser.add_argument("-tlr", type=float, default=DEFAULT_MAX_LOSS_RATE, 
                          help="最大丢包率 0-1")
        parser.add_argument("-sl", type=float, default=0, 
                          help="最小下载速度/MB/s")
        
        # 稳定性参数（新增）
        parser.add_argument("--max-cv", type=float, default=100,
                          help="最大变异系数/百分比 (默认100，建议20以下)")
        parser.add_argument("--min-stability", type=float, default=0,
                          help="最小稳定性得分 (0-100，默认0)")
        parser.add_argument("--sort", choices=['composite', 'speed', 'stability', 'latency'],
                          default='composite',
                          help="排序方式: composite综合/speed速度/stability稳定性/latency延迟")
        
        # 文件参数
        parser.add_argument("-f", default=DEFAULT_IP_FILE, 
                          help="IP列表文件")
        parser.add_argument("-o", "--output", default="",
                          help="保存结果 (支持.csv/.json)")
        parser.add_argument("-ip", default="", 
                          help="直接指定IP")
        
        # 控制参数
        parser.add_argument("-dd", action="store_true", 
                          help="禁用下载测试")
        parser.add_argument("-v", action="store_true", 
                          help="显示版本号")
        parser.add_argument("--debug", action="store_true",
                          help="显示调试信息")
        
        # 高级参数
        parser.add_argument("--max-ips", type=int, default=50000,
                          help="最多测试IP数")
        parser.add_argument("--sample-rate", type=float, default=1.0,
                          help="采样倍率 0.5-5.0")
        
        args = parser.parse_args()
        
        # 参数验证
        if not (args.url.startswith("http://") or args.url.startswith("https://")):
            parser.error("URL必须以http://或https://开头")
        if not 1 <= args.dc <= 20:
            parser.error("下载测试次数必须在1-20之间")
        
        return args
    
    def validate_and_warn(self):
        """验证参数并给出警告"""
        if self.args.async_ping and not ASYNC_AVAILABLE:
            print("❌ 错误: 异步功能需要Python 3.7+")
            sys.exit(1)
        
        # 稳定性测试建议
        if self.args.dc < 5:
            print(f"💡 提示: 当前下载测试次数为{self.args.dc}次")
            print(f"    建议使用 -dc 5 或更多次以获得更准确的稳定性分析\n")
    
    def select_ping_executor(self) -> PingExecutor:
        """选择Ping执行器"""
        if self.args.async_ping:
            return AsyncPingExecutor(self)
        else:
            return SyncPingExecutor(self)
    
    def select_download_executor(self) -> DownloadExecutor:
        """选择下载执行器"""
        return SyncSerialDownloadExecutor(self)
    
    def filter_latency_samples(self, ip: str, latencies: List[float]) -> List[float]:
        """过滤延迟样本"""
        if not latencies:
            return []
        return list(latencies)
    
    def aggregate_latency(self, latencies: List[float]) -> float:
        """聚合延迟值"""
        if not latencies:
            return 0.0
        return sum(latencies) / len(latencies)
    
    def update_result(self, ip: str, **kwargs):
        """统一的结果更新方法"""
        with self.lock:
            result = self.results_dict.get(ip)
            if result:
                for key, value in kwargs.items():
                    setattr(result, key, value)
    
    def update_best_ips_display(self):
        """更新实时最佳IP显示"""
        # 根据排序模式选择
        if self.args.sort == 'composite':
            sorted_results = sorted(
                [r for r in self.results if r.composite_score > 0],
                key=lambda x: x.composite_score,
                reverse=True
            )[:3]
            title = "综合得分"
        elif self.args.sort == 'speed':
            sorted_results = sorted(
                [r for r in self.results if r.download_speed > 0],
                key=lambda x: x.download_speed,
                reverse=True
            )[:3]
            title = "平均速度"
        elif self.args.sort == 'stability':
            sorted_results = sorted(
                [r for r in self.results if r.stability_score > 0],
                key=lambda x: x.stability_score,
                reverse=True
            )[:3]
            title = "稳定性"
        else:
            return
        
        if sorted_results:
            print(f"\n{'━'*70}")
            print(f"🏆 当前最佳IP (按{title}排序)")
            print(f"{'━'*70}")
            for i, r in enumerate(sorted_results, 1):
                print(f"{i}. {r.ip:<16} "
                      f"平均:{r.download_speed:>6.2f}MB/s  "
                      f"稳定:{r.get_stability_stars()}  "
                      f"综合:{r.composite_score:>6.2f}")
            print(f"{'━'*70}\n")
    
    def load_ip_ranges(self) -> List[str]:
        """加载IP范围"""
        if self.args.ip:
            return [x.strip() for x in self.args.ip.split(",")]
        else:
            if not os.path.exists(self.args.f):
                print(f"❌ 错误: IP文件 '{self.args.f}' 不存在!")
                sys.exit(1)
            with open(self.args.f, "r", encoding='utf-8') as f:
                return [line.strip() for line in f if line.strip() and not line.startswith("#")]
    
    def sample_ips_from_cidr(self, cidr: str, max_samples: int) -> Iterator[str]:
        """从CIDR范围生成IP样本"""
        try:
            network = ipaddress.ip_network(cidr, strict=False)
            total_hosts = network.num_addresses - 2
            
            if total_hosts <= 0:
                return
            
            max_samples = int(max_samples * self.sample_multiplier)
            
            # 简化采样策略
            if network.prefixlen <= 16:
                # 大网段：采样少量/24子网
                sample_count = min(max_samples // 2, 100)
                subnets = list(network.subnets(new_prefix=24))
                for subnet in random.sample(subnets, min(len(subnets), sample_count)):
                    hosts = list(subnet.hosts())
                    if hosts:
                        for _ in range(min(2, len(hosts))):
                            yield str(random.choice(hosts))
                            
            elif network.prefixlen <= 20:
                # 中等网段
                hosts = list(network.hosts())
                sample_count = min(50, len(hosts))
                for ip in random.sample(hosts, sample_count):
                    yield str(ip)
                    
            elif network.prefixlen <= 24:
                # 小网段
                hosts = list(network.hosts())
                sample_count = min(10, len(hosts))
                for ip in random.sample(hosts, sample_count):
                    yield str(ip)
            else:
                # 很小的网段
                hosts = list(network.hosts())
                sample_count = min(3, len(hosts))
                for ip in random.sample(hosts, sample_count):
                    yield str(ip)
                    
        except ValueError as e:
            if self.debug:
                print(f"跳过无效CIDR {cidr}: {e}")
    
    def generate_test_ips(self, ip_ranges: List[str]) -> Iterator[str]:
        """生成测试IP"""
        ip_count = 0
        for ip_range in ip_ranges:
            if ip_count >= self.args.max_ips:
                break
            
            if "/" in ip_range:
                # CIDR范围
                remaining = self.args.max_ips - ip_count
                for ip in self.sample_ips_from_cidr(ip_range, min(remaining, CIDR_SAMPLE_LIMIT)):
                    yield ip
                    ip_count += 1
                    if ip_count >= self.args.max_ips:
                        break
            else:
                # 单个IP
                try:
                    ipaddress.ip_address(ip_range)
                    yield ip_range
                    ip_count += 1
                except ValueError:
                    if self.debug:
                        print(f"跳过无效IP: {ip_range}")
    
    def tcp_ping(self, ip: str) -> Optional[Tuple[float, float]]:
        """TCP Ping测试"""
        success_count = 0
        latencies = []
        
        for i in range(self.args.t):
            start = time.perf_counter()
            try:
                with socket.create_connection((ip, self.args.tp), timeout=self.args.timeout):
                    latency_ms = (time.perf_counter() - start) * 1000
                    success_count += 1
                    latencies.append(latency_ms)
            except:
                if i == 0:
                    return None
                continue
        
        if success_count == 0:
            return None
        
        avg_latency = sum(latencies) / len(latencies)
        loss_rate = 1 - (success_count / self.args.t)
        return avg_latency, loss_rate
    
    def download_test_socket(self, ip: str) -> float:
        """Socket下载测试"""
        hostname = self.parsed_url.hostname
        port = self.args.tp
        path = self.parsed_url.path
        if self.parsed_url.query:
            path += f"?{self.parsed_url.query}"
        
        is_https = self.parsed_url.scheme == 'https'
        
        try:
            sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            sock.settimeout(DEFAULT_TIMEOUT)
            sock.connect((ip, port))
            
            if is_https:
                context = ssl.create_default_context()
                context.check_hostname = False
                context.verify_mode = ssl.CERT_NONE
                sock = context.wrap_socket(sock, server_hostname=hostname)
            
            request = (
                f"GET {path} HTTP/1.1\r\n"
                f"Host: {hostname}\r\n"
                f"User-Agent: Mozilla/5.0\r\n"
                f"Connection: close\r\n"
                f"\r\n"
            )
            sock.sendall(request.encode())
            
            downloaded = 0
            start_time = time.perf_counter()
            
            # 跳过HTTP头
            buffer = b""
            while b"\r\n\r\n" not in buffer:
                chunk = sock.recv(4096)
                if not chunk:
                    break
                buffer += chunk
            
            if b"\r\n\r\n" in buffer:
                header_end = buffer.find(b"\r\n\r\n") + 4
                downloaded = len(buffer) - header_end
            
            # 下载数据
            while time.perf_counter() - start_time < self.args.dt:
                try:
                    chunk = sock.recv(32768)
                    if not chunk:
                        break
                    downloaded += len(chunk)
                except:
                    break
            
            sock.close()
            
            elapsed = time.perf_counter() - start_time
            return (downloaded / elapsed / 1024 / 1024) if elapsed > 0 else 0.0
            
        except:
            return 0.0
    
    def print_detailed_results(self):
        """打印详细结果"""
        print(f"\n{'='*70}")
        print("📊 详细测试结果")
        print(f"{'='*70}\n")
        
        if not self.results:
            print("❌ 没有符合条件的IP!")
            return
        
        # 应用稳定性过滤
        filtered_results = [
            r for r in self.results 
            if r.speed_cv <= self.args.max_cv and 
               r.stability_score >= self.args.min_stability and
               r.download_speed > 0
        ]
        
        if not filtered_results:
            print(f"❌ 没有满足稳定性要求的IP!")
            print(f"   (最大CV: {self.args.max_cv}%, 最小稳定性: {self.args.min_stability})")
            return
        
        # 排序
        if self.args.sort == 'composite':
            filtered_results.sort(key=lambda x: x.composite_score, reverse=True)
            sort_name = "综合得分"
        elif self.args.sort == 'speed':
            filtered_results.sort(key=lambda x: x.download_speed, reverse=True)
            sort_name = "平均速度"
        elif self.args.sort == 'stability':
            filtered_results.sort(key=lambda x: x.stability_score, reverse=True)
            sort_name = "稳定性"
        elif self.args.sort == 'latency':
            filtered_results.sort(key=lambda x: x.latency)
            sort_name = "延迟"
        
        print(f"排序方式: {sort_name}\n")
        
        # 汇总表格
        print(f"{'IP地址':<16} {'平均速度':>10} {'速度范围':>18} {'波动':>8} {'稳定性':>10} {'综合':>8}")
        print(f"{'-'*16} {'-'*10} {'-'*18} {'-'*8} {'-'*10} {'-'*8}")
        
        display_count = min(self.args.dn, len(filtered_results))
        for result in filtered_results[:display_count]:
            speed_range = f"{result.speed_min:.1f}-{result.speed_max:.1f}"
            stars = result.get_stability_stars()
            
            print(f"{result.ip:<16} "
                  f"{result.download_speed:>9.2f}MB "
                  f"{speed_range:>18}MB "
                  f"{result.speed_cv:>7.1f}% "
                  f"{stars:>10} "
                  f"{result.composite_score:>8.1f}")
        
        print(f"{'='*70}\n")
        
        # 详细信息（Top 3）
        for rank, result in enumerate(filtered_results[:3], 1):
            medals = {1: "🥇", 2: "🥈", 3: "🥉"}
            print(f"{'━'*70}")
            print(f"{medals[rank]} 第{rank}名: {result.ip}")
            print(f"{'━'*70}")
            print(f"   平均速度: {result.download_speed:.2f} MB/s")
            print(f"   延迟: {result.latency:.2f}ms | 丢包率: {result.loss_rate:.1%}")
            print(f"   速度范围: {result.speed_min:.2f} - {result.speed_max:.2f} MB/s "
                  f"(波动 {result.speed_max - result.speed_min:.2f} MB/s)")
            
            # 波动警告
            if result.speed_cv > 15:
                print(f"   稳定性: {result.get_stability_stars()} "
                      f"(CV: {result.speed_cv:.1f}%) ⚠️  波动较大")
            else:
                print(f"   稳定性: {result.get_stability_stars()} "
                      f"(CV: {result.speed_cv:.1f}%)")
            
            print(f"   综合得分: {result.composite_score:.2f}")
            print(f"   推荐指数: {result.get_recommendation()}")
            
            # 测速明细可视化
            if len(result.download_speeds) > 1:
                print(f"\n   {len(result.download_speeds)}次测速明细:")
                print(f"   ┌{'─'*54}┐")
                
                max_speed = max(result.download_speeds)
                for i, speed in enumerate(result.download_speeds, 1):
                    if speed > 0:
                        bar_length = int((speed / max_speed) * 35)
                        bar = "█" * bar_length
                        print(f"   │ #{i:<2} {speed:>6.1f} MB/s  {bar:<35} │")
                    else:
                        print(f"   │ #{i:<2}   失败     {'':35} │")
                
                print(f"   └{'─'*54}┘")
            
            print()
        
        print(f"{'='*70}\n")
        
        # 统计信息
        print("📈 稳定性统计:")
        cv_ranges = {
            "非常稳定 (CV<5%)": len([r for r in filtered_results if r.speed_cv < 5]),
            "较稳定 (5%≤CV<10%)": len([r for r in filtered_results if 5 <= r.speed_cv < 10]),
            "一般 (10%≤CV<20%)": len([r for r in filtered_results if 10 <= r.speed_cv < 20]),
            "波动大 (CV≥20%)": len([r for r in filtered_results if r.speed_cv >= 20]),
        }
        for label, count in cv_ranges.items():
            if count > 0:
                print(f"   {label}: {count} 个IP")
        
        print(f"\n💡 说明:")
        print(f"   • 变异系数(CV) = 标准差/平均值 × 100%，越小越稳定")
        print(f"   • 综合得分 = 平均速度 × 稳定性权重，平衡速度和稳定性")
        print(f"   • 稳定性星级: ⭐⭐⭐⭐⭐(CV<3%) > ⭐⭐⭐⭐(CV<5%) > ⭐⭐⭐(CV<10%)")
        
        # 推荐IP
        if filtered_results:
            best = filtered_results[0]
            print(f"\n🎯 推荐使用: {best.ip}")
            print(f"   理由: {sort_name}最佳 - "
                  f"平均{best.download_speed:.2f}MB/s, "
                  f"稳定性{best.get_stability_stars()}, "
                  f"延迟{best.latency:.0f}ms")
    
    def save_results(self):
        """保存结果到文件"""
        if not self.args.output:
            return
        
        output_file = self.args.output
        
        try:
            if output_file.endswith('.csv'):
                self.save_to_csv(output_file)
            elif output_file.endswith('.json'):
                self.save_to_json(output_file)
            else:
                if '.' not in output_file:
                    output_file += '.csv'
                self.save_to_csv(output_file)
            
            print(f"\n💾 结果已保存到: {output_file}")
        except Exception as e:
            print(f"\n❌保存文件失败: {e}")
    
    def save_to_csv(self, filename: str):
        """保存为CSV格式"""
        # 应用稳定性过滤
        filtered_results = [
            r for r in self.results 
            if r.speed_cv <= self.args.max_cv and 
               r.stability_score >= self.args.min_stability
        ]
        
        # 排序
        if self.args.sort == 'composite':
            filtered_results.sort(key=lambda x: x.composite_score, reverse=True)
        elif self.args.sort == 'speed':
            filtered_results.sort(key=lambda x: x.download_speed, reverse=True)
        elif self.args.sort == 'stability':
            filtered_results.sort(key=lambda x: x.stability_score, reverse=True)
        elif self.args.sort == 'latency':
            filtered_results.sort(key=lambda x: x.latency)
        
        with open(filename, 'w', newline='', encoding='utf-8') as f:
            writer = csv.writer(f)
            writer.writerow([
                'IP地址', '延迟(ms)', '丢包率(%)', '平均速度(MB/s)', 
                '最小速度(MB/s)', '最大速度(MB/s)', '标准差', '变异系数(%)',
                '稳定性得分', '综合得分', '测试次数', '各次速度', '测试时间'
            ])
            
            for result in filtered_results[:self.args.dn]:
                speeds_str = ','.join(f"{s:.2f}" for s in result.download_speeds)
                writer.writerow([
                    result.ip,
                    f"{result.latency:.2f}",
                    f"{result.loss_rate*100:.1f}",
                    f"{result.download_speed:.2f}",
                    f"{result.speed_min:.2f}",
                    f"{result.speed_max:.2f}",
                    f"{result.speed_std:.2f}",
                    f"{result.speed_cv:.2f}",
                    f"{result.stability_score:.2f}",
                    f"{result.composite_score:.2f}",
                    len(result.download_speeds),
                    speeds_str,
                    result.test_time
                ])
    
    def save_to_json(self, filename: str):
        """保存为JSON格式"""
        # 应用稳定性过滤和排序
        filtered_results = [
            r for r in self.results 
            if r.speed_cv <= self.args.max_cv and 
               r.stability_score >= self.args.min_stability
        ]
        
        if self.args.sort == 'composite':
            filtered_results.sort(key=lambda x: x.composite_score, reverse=True)
        elif self.args.sort == 'speed':
            filtered_results.sort(key=lambda x: x.download_speed, reverse=True)
        elif self.args.sort == 'stability':
            filtered_results.sort(key=lambda x: x.stability_score, reverse=True)
        elif self.args.sort == 'latency':
            filtered_results.sort(key=lambda x: x.latency)
        
        data = {
            'test_info': {
                'version': VERSION,
                'test_time': datetime.now().strftime("%Y-%m-%d %H:%M:%S"),
                'total_ips_tested': self.total_ips_tested,
                'download_tests_per_ip': self.args.dc,
                'sort_by': self.args.sort,
                'max_cv': self.args.max_cv,
                'min_stability': self.args.min_stability,
            },
            'results': []
        }
        
        for result in filtered_results[:self.args.dn]:
            data['results'].append({
                'ip': result.ip,
                'latency_ms': round(result.latency, 2),
                'loss_rate': round(result.loss_rate, 4),
                'avg_speed_mbps': round(result.download_speed, 2),
                'min_speed_mbps': round(result.speed_min, 2),
                'max_speed_mbps': round(result.speed_max, 2),
                'std_dev': round(result.speed_std, 2),
                'cv_percent': round(result.speed_cv, 2),
                'stability_score': round(result.stability_score, 2),
                'composite_score': round(result.composite_score, 2),
                'all_speeds_mbps': [round(s, 2) for s in result.download_speeds],
                'test_time': result.test_time
            })
        
        with open(filename, 'w', encoding='utf-8') as f:
            json.dump(data, f, indent=2, ensure_ascii=False)
    
    def run_tests(self) -> None:
        """执行测试"""
        print(f"{'='*70}")
        print(f"🚀 CloudflareSpeedTest {VERSION}")
        print(f"{'='*70}")
        
        ip_ranges = self.load_ip_ranges()
        print(f"📂 加载了 {len(ip_ranges)} 个IP范围")
        print(f"⚙️  最大测试数: {self.args.max_ips} 个IP")
        print(f"⚙️  下载测试: 每IP测 {self.args.dc} 次 (稳定性分析)")
        print(f"⚙️  排序方式: {self.args.sort}")
        
        if self.args.async_ping:
            print(f"⚙️  Ping模式: 异步 (并发{self.args.async_ping_limit}) 🚀")
        else:
            print(f"⚙️  Ping模式: 同步 (并发{self.args.n}线程)")
        print()
        
        # 阶段1: 延迟测试
        print(f"{'='*70}")
        print("🔍 阶段 1/2: 延迟测试")
        print(f"{'='*70}\n")
        
        ip_list = list(self.generate_test_ips(ip_ranges))
        self.total_ips_tested = len(ip_list)
        
        batch_results = self.ping_executor.execute(ip_list)
        
        with self.lock:
            self.results.extend(batch_results)
            for r in batch_results:
                self.results_dict[r.ip] = r
        
        self.results.sort(key=lambda x: x.latency)
        
        print(f"\n✅ 延迟测试完成: {len(self.results)} 个IP通过筛选")
        print(f"📊 共测试: {self.total_ips_tested} 个IP\n")
        
        # 阶段2: 下载测试
        if not self.args.dd and self.results:
            print(f"{'='*70}")
            print("🚀 阶段 2/2: 下载速度测试（含稳定性分析）")
            print(f"{'='*70}\n")
            
            test_ips = [res.ip for res in self.results[:self.args.dn]]
            self.download_executor.execute(test_ips)
            
            print(f"\n✅ 下载测试完成!\n")
    
    def main(self) -> None:
        """主函数"""
        if self.args.v:
            print(f"CloudflareSpeedTest {VERSION}")
            return
        
        try:
            start_time = time.time()
            self.run_tests()
            elapsed = time.time() - start_time
            
            self.print_detailed_results()
            self.save_results()
            
            print(f"\n⏱️  总耗时: {elapsed:.1f}秒")
            print(f"{'='*70}\n")
            
        except KeyboardInterrupt:
            print("\n\n⚠️  程序被用户中断!")
            if self.results and self.args.output:
                print("💾 保存已测试的结果...")
                self.save_results()
            sys.exit(0)
        except Exception as e:
            print(f"\n❌ 意外错误: {e}")
            import traceback
            traceback.print_exc()
            sys.exit(1)


if __name__ == "__main__":
    tester = CloudflareSpeedTest()
    tester.main()