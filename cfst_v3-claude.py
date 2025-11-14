#!/usr/bin/env python3
"""
CloudflareSpeedTest Asyncio版 v3.5
新功能：
- 异步Ping测试（速度提升2-5倍）
- 异步下载测试（可选）
- 独立参数控制
- 保证下载测速准确性（默认单线程串行）
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
from abc import ABC, abstractmethod
from concurrent.futures import ThreadPoolExecutor
from typing import Optional, Tuple, List, Iterator, Dict
from dataclasses import dataclass
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
DEFAULT_DOWNLOAD_COUNT = 5
DEFAULT_DOWNLOAD_SECONDS = 10
DEFAULT_IP_FILE = "ip.txt"
DEFAULT_TIMEOUT = 5
DEFAULT_MAX_LOSS_RATE = 0.0
DEFAULT_ASYNC_LIMIT = 512
MAX_SAFE_ASYNC_LIMIT = 2048
VERSION = "v3.5-asyncio"

# 采样限制
CIDR_SAMPLE_LIMIT = 1000

@dataclass
class TestResult:
    """测试结果数据类"""
    ip: str
    latency: float
    loss_rate: float
    download_speed: float = 0.0
    download_speeds: List[float] = None
    test_time: str = ""
    
    def __post_init__(self):
        if self.download_speeds is None:
            self.download_speeds = []
        if not self.test_time:
            self.test_time = datetime.now().strftime("%Y-%m-%d %H:%M:%S")


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
        consecutive_failures = 0
        
        for i in range(self.tester.args.t):
            try:
                async with semaphore:
                    # 计时从获得信号量之后开始，只测量实际连接时间
                    start = time.perf_counter()
                    reader, writer = await asyncio.wait_for(
                        asyncio.open_connection(ip, self.tester.args.tp),
                        timeout=timeout
                    )
                    latency_ms = (time.perf_counter() - start) * 1000
                    
                    # 立即关闭连接（在semaphore保护内）
                    try:
                        writer.close()
                        await writer.wait_closed()
                    except:
                        pass
                
                success_count += 1
                latencies.append(latency_ms)
                consecutive_failures = 0  # 重置连续失败计数
                
                if self.tester.debug:
                    print(f"      [{ip}] 第{i+1}次: {latency_ms:.2f}ms")
                
            except (asyncio.TimeoutError, OSError, ConnectionRefusedError) as e:
                consecutive_failures += 1
                if self.tester.debug:
                    print(f"      [{ip}] 第{i+1}次: 失败 ({type(e).__name__})")
                
                # 如果前2次都失败，直接放弃该IP
                if consecutive_failures >= 2 and success_count == 0:
                    if self.tester.debug:
                        print(f"      [{ip}] 连续失败{consecutive_failures}次，放弃测试")
                    return None
                continue
            finally:
                if i < self.tester.args.t - 1:
                    await asyncio.sleep(0.001)  # 1ms延迟，避免连接风暴
        
        if success_count == 0:
            return None
        
        filtered_latencies = self.tester.filter_latency_samples(ip, latencies)
        avg_latency = self.tester.aggregate_latency(filtered_latencies)
        loss_rate = 1 - (success_count / self.tester.args.t)
        
        if self.tester.debug and filtered_latencies:
            min_lat = min(filtered_latencies)
            max_lat = max(filtered_latencies)
            print(f"      [{ip}] 平均:{avg_latency:.2f}ms, 最小:{min_lat:.2f}ms, 最大:{max_lat:.2f}ms, 成功:{success_count}/{self.tester.args.t}")
        
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
        print(f"⚙️  每IP测试时长: {self.tester.args.dt}秒")
        print(f"⚙️  预计总耗时: {len(test_ips) * self.tester.args.dt}秒\n")
        
        for i, ip in enumerate(test_ips, 1):
            progress = f"[{i}/{len(test_ips)}]"
            percentage = f"({i*100//len(test_ips)}%)"
            
            print(f"{progress} {percentage} 测试 {ip}...")
            
            # 执行下载测试
            speed = self.tester.download_test(ip)
            results[ip] = speed
            
            # 更新结果
            self.tester.update_result(ip, download_speed=speed)
            
            # 显示结果
            if speed > 0:
                if self.tester.args.dc > 1:
                    print(f"    ✅ 平均速度: {speed:.2f} MB/s")
                else:
                    print(f"    ✅ 速度: {speed:.2f} MB/s")
            else:
                print(f"    ❌ 下载失败")
            
            # 显示剩余时间
            if i < len(test_ips):
                remaining = (len(test_ips) - i) * self.tester.args.dt
                print(f"    ⏱️  剩余时间: 约{remaining}秒\n")
            else:
                print()
            
            # 实时最佳IP
            self.tester.update_best_ips_display()
        
        return results


class AsyncSerialDownloadExecutor(DownloadExecutor):
    """异步串行下载执行器（较准确）"""
    
    def __init__(self, tester):
        self.tester = tester
    
    async def download_test_async(self, ip: str) -> float:
        """异步下载测试"""
        hostname = self.tester.parsed_url.hostname
        port = self.tester.args.tp
        path = self.tester.parsed_url.path
        if self.tester.parsed_url.query:
            path += f"?{self.tester.parsed_url.query}"
        
        is_https = self.tester.parsed_url.scheme == 'https'
        
        try:
            # 建立连接
            reader, writer = await asyncio.wait_for(
                asyncio.open_connection(ip, port),
                timeout=DEFAULT_TIMEOUT
            )
            
            # HTTPS需要SSL包装
            if is_https:
                ssl_context = ssl.create_default_context()
                ssl_context.check_hostname = False
                ssl_context.verify_mode = ssl.CERT_NONE
                
                # 升级到SSL
                transport = writer.transport
                protocol = transport.get_protocol()
                new_transport = await asyncio.wait_for(
                    asyncio.get_event_loop().start_tls(
                        transport, protocol, ssl_context, server_hostname=hostname
                    ),
                    timeout=DEFAULT_TIMEOUT
                )
                reader = asyncio.StreamReader()
                reader.set_transport(new_transport)
                writer = asyncio.StreamWriter(new_transport, protocol, reader, asyncio.get_event_loop())
            
            # 发送HTTP请求
            request = (
                f"GET {path} HTTP/1.1\r\n"
                f"Host: {hostname}\r\n"
                f"User-Agent: Mozilla/5.0\r\n"
                f"Accept: */*\r\n"
                f"Connection: close\r\n"
                f"\r\n"
            )
            writer.write(request.encode())
            await writer.drain()
            
            # 跳过HTTP头
            header_received = False
            downloaded = 0
            while not header_received:
                line = await reader.readline()
                if line == b"\r\n":
                    header_received = True
                    break
            
            # 下载数据
            start_time = time.perf_counter()
            while True:
                if time.perf_counter() - start_time > self.tester.args.dt:
                    break
                try:
                    chunk = await asyncio.wait_for(reader.read(32768), timeout=1.0)
                    if not chunk:
                        break
                    downloaded += len(chunk)
                except asyncio.TimeoutError:
                    break
            
            writer.close()
            await writer.wait_closed()
            
            elapsed = time.perf_counter() - start_time
            speed_mbps = (downloaded / elapsed / 1024 / 1024) if elapsed > 0 else 0
            return speed_mbps
            
        except Exception as e:
            self.tester.debug_print(f"{ip} 异步下载失败: {e}")
            return 0.0
    
    async def execute_async(self, test_ips: List[str]) -> Dict[str, float]:
        """异步串行下载测试"""
        results = {}
        
        print(f"📥 下载模式: 异步串行（单线程，较准确）")
        print(f"⚙️  每IP测试时长: {self.tester.args.dt}秒")
        print(f"⚙️  预计总耗时: {len(test_ips) * self.tester.args.dt}秒\n")
        
        for i, ip in enumerate(test_ips, 1):
            progress = f"[{i}/{len(test_ips)}]"
            percentage = f"({i*100//len(test_ips)}%)"
            
            print(f"{progress} {percentage} 测试 {ip}...")
            
            # 异步下载测试（但仍然串行）
            speed = await self.download_test_async(ip)
            results[ip] = speed
            
            # 更新结果
            self.tester.update_result(ip, download_speed=speed)
            
            # 显示结果
            if speed > 0:
                print(f"    ✅ 速度: {speed:.2f} MB/s")
            else:
                print(f"    ❌ 下载失败")
            
            # 显示剩余时间
            if i < len(test_ips):
                remaining = (len(test_ips) - i) * self.tester.args.dt
                print(f"    ⏱️  剩余时间: 约{remaining}秒\n")
            else:
                print()
            
            # 实时最佳IP
            self.tester.update_best_ips_display()
        
        return results
    
    def execute(self, test_ips: List[str]) -> Dict[str, float]:
        """同步接口"""
        return asyncio.run(self.execute_async(test_ips))


class AsyncParallelDownloadExecutor(DownloadExecutor):
    """异步并行下载执行器（最快但不准确）"""
    
    def __init__(self, tester):
        self.tester = tester
        self.async_serial = AsyncSerialDownloadExecutor(tester)
    
    async def execute_async(self, test_ips: List[str]) -> Dict[str, float]:
        """异步并行下载测试"""
        print(f"📥 下载模式: 异步并行（多IP同时测试）")
        print(f"⚠️  警告: 并行下载会导致多个IP抢占带宽，测速结果可能不准确！")
        print(f"⚙️  并发数: {len(test_ips)}个IP同时测试")
        print(f"⚙️  预计耗时: {self.tester.args.dt}秒\n")
        
        # 并发执行所有下载
        tasks = [self.async_serial.download_test_async(ip) for ip in test_ips]
        speeds = await asyncio.gather(*tasks)
        
        results = dict(zip(test_ips, speeds))
        
        # 更新所有结果
        for ip, speed in results.items():
            self.tester.update_result(ip, download_speed=speed)
        
        return results
    
    def execute(self, test_ips: List[str]) -> Dict[str, float]:
        """同步接口"""
        return asyncio.run(self.execute_async(test_ips))


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
            description=f"Cloudflare CDN IP 测速工具 {VERSION}",
            formatter_class=argparse.RawDescriptionHelpFormatter,
            epilog="""
示例:
  标准测试（推荐）:
    python3 %(prog)s --async-ping --max-ips 50000 -dn 10 -o result.csv
  
  只测Ping:
    python3 %(prog)s --async-ping --max-ips 100000 -dd -o ping.csv
  
  实验性快速测试:
    python3 %(prog)s --async-all --parallel-download
            """
        )
        # 基础参数
        parser.add_argument("-n", type=int, default=DEFAULT_THREADS, 
                          help="同步模式并发线程数 (默认200)")
        parser.add_argument("-t", type=int, default=DEFAULT_TEST_COUNT, 
                          help="Ping测试次数 (默认3)")
        parser.add_argument("-dn", type=int, default=DEFAULT_DOWNLOAD_COUNT, 
                          help="下载测试IP数量 (默认5)")
        parser.add_argument("-dt", type=int, default=DEFAULT_DOWNLOAD_SECONDS, 
                          help="下载测试时长/秒 (默认10)")
        parser.add_argument("-dc", type=int, default=1,
                          help="下载测试次数/取平均 (默认1)")
        parser.add_argument("-tp", type=int, default=DEFAULT_PORT, 
                          help="测试端口 (默认443)")
        parser.add_argument("-url", default=DEFAULT_URL, 
                          help="下载测试URL")
        parser.add_argument("--timeout", type=int, default=DEFAULT_TIMEOUT,
                          help="连接超时时间/秒 (默认5，建议3-10)")
        
        # 异步参数（新增）
        parser.add_argument("--async-ping", action="store_true",
                          help="使用异步IO进行Ping测试（速度提升2-5倍）")
        parser.add_argument("--async-ping-limit", type=int, default=DEFAULT_ASYNC_LIMIT,
                          help=f"异步Ping最大并发数 (默认{DEFAULT_ASYNC_LIMIT})")
        parser.add_argument("--async-download", action="store_true",
                          help="使用异步IO进行下载测试（实验性）")
        parser.add_argument("--parallel-download", action="store_true",
                          help="并行下载测试（需配合--async-download，会影响准确性）")
        parser.add_argument("--async-all", action="store_true",
                          help="启用所有异步功能")
        
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
        parser.add_argument("-allip", action="store_true", 
                          help="测试所有IP（慎用）")
        parser.add_argument("-v", action="store_true", 
                          help="显示版本号")
        parser.add_argument("--debug", action="store_true",
                          help="显示调试信息")
        
        # 高级参数
        parser.add_argument("--max-ips", type=int, default=50000,
                          help="最多测试IP数")
        parser.add_argument("--batch-size", type=int, default=5000,
                          help="批处理大小")
        parser.add_argument("--sample-rate", type=float, default=1.0,
                          help="采样倍率 0.5-5.0")
        
        args = parser.parse_args()
        
        # 参数验证
        if not (args.url.startswith("http://") or args.url.startswith("https://")):
            parser.error("URL必须以http://或https://开头")
        if not 1 <= args.n <= 500:
            parser.error("线程数必须在1-500之间")
        if not 1 <= args.dc <= 5:
            parser.error("下载测试次数必须在1-5之间")
        if not 0.5 <= args.sample_rate <= 5.0:
            parser.error("采样倍率必须在0.5-5.0之间")
        if not 1 <= args.timeout <= 30:
            parser.error("超时时间必须在1-30秒之间")
        
        return args
    
    def validate_and_warn(self):
        """验证参数并给出警告"""
        # 处理--async-all
        if self.args.async_all:
            self.args.async_ping = True
            self.args.async_download = True
        
        # 检查异步支持
        if (self.args.async_ping or self.args.async_download) and not ASYNC_AVAILABLE:
            print("❌ 错误: 异步功能需要Python 3.7+")
            sys.exit(1)
        
        if self.args.async_ping:
            if self.args.async_ping_limit <= 0:
                print(f"⚠️  警告: --async-ping-limit 必须大于0，已恢复为默认值 {DEFAULT_ASYNC_LIMIT}")
                self.args.async_ping_limit = DEFAULT_ASYNC_LIMIT
            elif self.args.async_ping_limit > MAX_SAFE_ASYNC_LIMIT:
                print(
                    f"⚠️  警告: --async-ping-limit={self.args.async_ping_limit} 并发数过高，"
                    "会显著抬高延迟测量值，已自动限制为 "
                    f"{MAX_SAFE_ASYNC_LIMIT}"
                )
                self.args.async_ping_limit = MAX_SAFE_ASYNC_LIMIT
        
        # 并行下载需要异步下载
        if self.args.parallel_download and not self.args.async_download:
            print("⚠️  警告: --parallel-download 需要配合 --async-download 使用")
            print("    已自动启用 --async-download\n")
            self.args.async_download = True
        
        # 并行下载警告
        if self.args.parallel_download:
            print("⚠️  警告: 并行下载测试会导致速度不准确（多个IP抢占带宽）")
            print("    建议: 移除 --parallel-download 参数以获得准确结果\n")
        
        # 提示异步Ping的优势
        if not self.args.async_ping and self.args.max_ips > 10000:
            print(f"💡 提示: 检测到需测试大量IP ({self.args.max_ips})")
            print("    建议使用 --async-ping 参数加速Ping测试（2-5倍提升）\n")
    
    def select_ping_executor(self) -> PingExecutor:
        """选择Ping执行器"""
        if self.args.async_ping:
            return AsyncPingExecutor(self)
        else:
            return SyncPingExecutor(self)
    
    def select_download_executor(self) -> DownloadExecutor:
        """选择下载执行器"""
        if self.args.async_download:
            if self.args.parallel_download:
                return AsyncParallelDownloadExecutor(self)
            else:
                return AsyncSerialDownloadExecutor(self)
        else:
            return SyncSerialDownloadExecutor(self)
    
    def debug_print(self, msg: str):
        """调试信息打印"""
        if self.debug:
            print(f"[DEBUG] {msg}")
    
    def filter_latency_samples(self, ip: str, latencies: List[float]) -> List[float]:
        """对延迟样本进行多阶段过滤，去除调度造成的异常高值"""
        if not latencies:
            return []
        filtered = list(latencies)
        
        # IQR过滤（针对偶发尖刺）
        if len(filtered) >= 10:
            sorted_latencies = sorted(filtered)
            q1_idx = len(sorted_latencies) // 4
            q3_idx = (len(sorted_latencies) * 3) // 4
            q1 = sorted_latencies[q1_idx]
            q3 = sorted_latencies[q3_idx]
            iqr = q3 - q1
            upper_bound = q3 + 1.5 * iqr
            iqr_filtered = [lat for lat in filtered if lat <= upper_bound]
            if iqr_filtered and len(iqr_filtered) < len(filtered):
                if self.debug:
                    print(f"      [{ip}] 过滤了 {len(filtered) - len(iqr_filtered)} 个异常值（阈值:{upper_bound:.2f}ms）")
                filtered = iqr_filtered
        
        if filtered:
            # 针对异步高并发引起的整体抬升，使用动态阈值再次清洗
            min_latency = min(filtered)
            dynamic_window = max(15.0, min_latency * 0.5)
            dynamic_threshold = min_latency + dynamic_window
            dynamic_filtered = [lat for lat in filtered if lat <= dynamic_threshold]
            if dynamic_filtered and len(dynamic_filtered) < len(filtered):
                if self.debug:
                    print(f"      [{ip}] 移除了 {len(filtered) - len(dynamic_filtered)} 个明显偏高的样本（动态阈值:{dynamic_threshold:.2f}ms）")
                filtered = dynamic_filtered
        
        return filtered if filtered else list(latencies)
    
    def aggregate_latency(self, latencies: List[float]) -> float:
        """将过滤后的延迟样本压缩成代表值"""
        if not latencies:
            return 0.0
        sorted_latencies = sorted(latencies)
        if len(sorted_latencies) <= 2:
            return sum(sorted_latencies) / len(sorted_latencies)
        span = sorted_latencies[-1] - sorted_latencies[0]
        tolerance = max(15.0, sorted_latencies[0] * 0.4)
        if span <= tolerance:
            return sum(sorted_latencies) / len(sorted_latencies)
        take_count = max(1, int(math.ceil(len(sorted_latencies) * 0.3)))
        take_count = min(take_count, len(sorted_latencies))
        trimmed = sorted_latencies[:take_count]
        return sum(trimmed) / len(trimmed)
    
    def update_result(self, ip: str, **kwargs):
        """统一的结果更新方法"""
        with self.lock:
            result = self.results_dict.get(ip)
            if result:
                for key, value in kwargs.items():
                    setattr(result, key, value)
    
    def update_best_ips_display(self):
        """更新实时最佳IP显示"""
        sorted_results = sorted(
            [r for r in self.results if r.download_speed > 0],
            key=lambda x: x.download_speed,
            reverse=True
        )[:3]
        
        if sorted_results:
            print(f"\n{'━'*50}")
            print(f"🏆 当前最佳IP (实时更新)")
            print(f"{'━'*50}")
            for i, result in enumerate(sorted_results, 1):
                speeds_info = ""
                if len(result.download_speeds) > 1:
                    speeds_str = ", ".join(f"{s:.1f}" for s in result.download_speeds)
                    speeds_info = f" [{speeds_str}]"
                print(f"{i}. {result.ip:<16} "
                      f"{result.download_speed:>6.2f}MB/s  "
                      f"({result.latency:.0f}ms){speeds_info}")
            print(f"{'━'*50}\n")
    
    # [采样、IP加载等方法保持不变，从v3.4复制]
    def sample_ips_from_cidr(self, cidr: str, max_samples: int) -> Iterator[str]:
        """从CIDR范围生成IP样本"""
        try:
            network = ipaddress.ip_network(cidr, strict=False)
            total_hosts = network.num_addresses - 2
            
            if total_hosts <= 0:
                return
            
            max_samples = int(max_samples * self.sample_multiplier)
            
            if self.args.allip:
                actual_samples = min(max_samples, total_hosts)
                self.debug_print(f"{cidr}: 全测试模式，采样 {actual_samples}/{total_hosts}")
                
                hosts = list(network.hosts())
                if len(hosts) <= max_samples:
                    for ip in hosts:
                        yield str(ip)
                else:
                    for ip in random.sample(hosts, max_samples):
                        yield str(ip)
            else:
                if network.prefixlen <= 16:
                    sample_count = int(min(max_samples // 2, 256) * self.sample_multiplier)
                    subnets = list(network.subnets(new_prefix=24))
                    for subnet in random.sample(subnets, min(len(subnets), sample_count)):
                        hosts = list(subnet.hosts())
                        if hosts:
                            samples_per_subnet = max(1, int(2 * self.sample_multiplier))
                            for _ in range(min(samples_per_subnet, len(hosts))):
                                yield str(random.choice(hosts))
                                
                elif network.prefixlen <= 20:
                    hosts = list(network.hosts())
                    sample_count = int(min(50, len(hosts)) * self.sample_multiplier)
                    for ip in random.sample(hosts, min(sample_count, len(hosts))):
                        yield str(ip)
                        
                elif network.prefixlen <= 24:
                    hosts = list(network.hosts())
                    sample_count = int(min(10, len(hosts)) * self.sample_multiplier)
                    for ip in random.sample(hosts, min(sample_count, len(hosts))):
                        yield str(ip)
                else:
                    hosts = list(network.hosts())
                    sample_count = int(min(3, len(hosts)) * self.sample_multiplier)
                    for ip in random.sample(hosts, min(sample_count, len(hosts))):
                        yield str(ip)
                        
        except ValueError as e:
            self.debug_print(f"跳过无效CIDR {cidr}: {e}")
    
    def load_ip_ranges(self) -> List[str]:
        """加载IP范围列表"""
        if self.args.ip:
            return [x.strip() for x in self.args.ip.split(",")]
        else:
            if not os.path.exists(self.args.f):
                print(f"❌ 错误: IP文件 '{self.args.f}' 不存在!")
                sys.exit(1)
            with open(self.args.f, "r", encoding='utf-8') as f:
                return [line.strip() for line in f if line.strip() and not line.startswith("#")]
    
    def generate_test_ips(self, ip_ranges: List[str]) -> Iterator[str]:
        """生成器：逐个产生待测试的IP"""
        ip_count = 0
        
        for ip_range in ip_ranges:
            if ip_count >= self.args.max_ips:
                self.debug_print(f"达到最大IP数量限制 {self.args.max_ips}")
                break
                
            if "/" in ip_range:
                remaining = self.args.max_ips - ip_count
                for ip in self.sample_ips_from_cidr(ip_range, min(remaining, CIDR_SAMPLE_LIMIT)):
                    yield ip
                    ip_count += 1
                    if ip_count >= self.args.max_ips:
                        break
            else:
                try:
                    ipaddress.ip_address(ip_range)
                    yield ip_range
                    ip_count += 1
                except ValueError:
                    self.debug_print(f"跳过无效IP: {ip_range}")
    
    def tcp_ping(self, ip: str) -> Optional[Tuple[float, float]]:
        """同步TCP连接测试"""
        success_count = 0
        latencies = []
        timeout = self.args.timeout
        consecutive_failures = 0
        
        for i in range(self.args.t):
            start = time.perf_counter()
            try:
                with socket.create_connection((ip, self.args.tp), timeout=timeout):
                    latency_ms = (time.perf_counter() - start) * 1000
                    success_count += 1
                    latencies.append(latency_ms)
                    consecutive_failures = 0  # 重置连续失败计数
                    
                    # ✅ 添加调试输出
                    if self.debug:
                        print(f"      [{ip}] 第{i+1}次: {latency_ms:.2f}ms")
                        
            except (socket.timeout, socket.error, OSError) as e:
                consecutive_failures += 1
                if self.debug:
                    print(f"      [{ip}] 第{i+1}次: 失败 ({type(e).__name__})")
                
                # 如果前2次都失败，直接放弃该IP
                if consecutive_failures >= 2 and success_count == 0:
                    if self.debug:
                        print(f"      [{ip}] 连续失败{consecutive_failures}次，放弃测试")
                    return None
                continue
        
        if success_count == 0:
            return None
        
        filtered_latencies = self.filter_latency_samples(ip, latencies)
        avg_latency = self.aggregate_latency(filtered_latencies)
        loss_rate = 1 - (success_count / self.args.t)
        
        # ✅ 添加汇总信息
        if self.debug and filtered_latencies:
            min_lat = min(filtered_latencies)
            max_lat = max(filtered_latencies)
            print(f"      [{ip}] 平均:{avg_latency:.2f}ms, 最小:{min_lat:.2f}ms, 最大:{max_lat:.2f}ms, 成功:{success_count}/{self.args.t}")
        
        return avg_latency, loss_rate
    
    def http_ping(self, ip: str) -> Optional[Tuple[float, float]]:
        """同步HTTP请求测试"""
        scheme = self.parsed_url.scheme
        hostname = self.parsed_url.hostname
        path = self.parsed_url.path or "/"
        
        url = f"{scheme}://{ip}:{self.args.tp}{path}"
        headers = {'Host': hostname}
        timeout = self.args.timeout  # ✅ 使用参数指定的超时时间
        
        success_count = 0
        latencies = []
        consecutive_failures = 0
        
        for i in range(self.args.t):
            start = time.perf_counter()
            try:
                response = requests.head(
                    url, headers=headers, timeout=timeout,
                    verify=False, allow_redirects=False
                )
                latency_ms = (time.perf_counter() - start) * 1000
                
                if str(response.status_code) in self.valid_http_codes:
                    success_count += 1
                    latencies.append(latency_ms)
                    consecutive_failures = 0  # 重置连续失败计数
                    
                    if self.debug:
                        print(f"      [{ip}] 第{i+1}次: {latency_ms:.2f}ms")
                    
                    if self.args.cfcolo:
                        colo = response.headers.get('CF-RAY', '').split('-')[-1]
                        if colo.upper() != self.args.cfcolo.upper():
                            return None
                else:
                    consecutive_failures += 1
            except (requests.RequestException, OSError):
                consecutive_failures += 1
                if self.debug:
                    print(f"      [{ip}] 第{i+1}次: HTTP请求失败")
                
                # 如果前2次都失败，直接放弃该IP
                if consecutive_failures >= 2 and success_count == 0:
                    if self.debug:
                        print(f"      [{ip}] 连续失败{consecutive_failures}次，放弃测试")
                    return None
                continue
        
        if success_count == 0:
            return None
        
        filtered_latencies = self.filter_latency_samples(ip, latencies)
        avg_latency = self.aggregate_latency(filtered_latencies)
        loss_rate = 1 - (success_count / self.args.t)
        
        if self.debug and filtered_latencies:
            min_lat = min(filtered_latencies)
            max_lat = max(filtered_latencies)
            print(f"      [{ip}] 平均:{avg_latency:.2f}ms, 最小:{min_lat:.2f}ms, 最大:{max_lat:.2f}ms, 成功:{success_count}/{self.args.t}")
        
        return avg_latency, loss_rate
    
    def download_test_socket(self, ip: str) -> float:
        """同步socket下载测试"""
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
                f"Accept: */*\r\n"
                f"Connection: close\r\n"
                f"\r\n"
            )
            sock.sendall(request.encode())
            
            downloaded = 0
            start_time = time.perf_counter()
            
            # 跳过HTTP头
            header_received = False
            buffer = b""
            while not header_received:
                chunk = sock.recv(4096)
                if not chunk:
                    break
                buffer += chunk
                if b"\r\n\r\n" in buffer:
                    header_end = buffer.find(b"\r\n\r\n") + 4
                    downloaded = len(buffer) - header_end
                    header_received = True
                    break
            
            # 下载数据
            while True:
                if time.perf_counter() - start_time > self.args.dt:
                    break
                try:
                    chunk = sock.recv(32768)
                    if not chunk:
                        break
                    downloaded += len(chunk)
                except socket.timeout:
                    break
            
            sock.close()
            
            elapsed = time.perf_counter() - start_time
            speed_mbps = (downloaded / elapsed / 1024 / 1024) if elapsed > 0 else 0
            return speed_mbps
            
        except (socket.error, OSError, ssl.SSLError) as e:
            self.debug_print(f"{ip} Socket下载失败: {e}")
            return 0.0
    
    def download_test(self, ip: str) -> float:
        """下载速度测试 - 支持多次测试取平均"""
        speeds = []
        
        for i in range(self.args.dc):
            if self.args.dc > 1:
                print(f"    第 {i+1}/{self.args.dc} 次测试...", end=" ")
            
            speed = self.download_test_socket(ip)
            speeds.append(speed)
            
            if self.args.dc > 1:
                print(f"{speed:.2f} MB/s")
            
            if speed == 0 and i == 0:
                break
            
            if i < self.args.dc - 1 and speed > 0:
                time.sleep(0.5)
        
        valid_speeds = [s for s in speeds if s > 0]
        if not valid_speeds:
            return 0.0
        
        avg_speed = sum(valid_speeds) / len(valid_speeds)
        self.update_result(ip, download_speeds=speeds)
        
        return avg_speed
    
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
            print(f"\n❌ 保存文件失败: {e}")
    
    def save_to_csv(self, filename: str):
        """保存为CSV格式"""
        with open(filename, 'w', newline='', encoding='utf-8') as f:
            writer = csv.writer(f)
            writer.writerow([
                'IP地址', '延迟(ms)', '丢包率(%)', '平均速度(MB/s)', 
                '测试次数', '各次速度(MB/s)', '测试时间'
            ])
            
            for result in self.results[:self.args.dn]:
                speeds_str = ','.join(f"{s:.2f}" for s in result.download_speeds) if result.download_speeds else ''
                writer.writerow([
                    result.ip,
                    f"{result.latency:.2f}",
                    f"{result.loss_rate*100:.1f}",
                    f"{result.download_speed:.2f}",
                    len(result.download_speeds) if result.download_speeds else 0,
                    speeds_str,
                    result.test_time
                ])
    
    def save_to_json(self, filename: str):
        """保存为JSON格式"""
        data = {
            'test_info': {
                'version': VERSION,
                'test_time': datetime.now().strftime("%Y-%m-%d %H:%M:%S"),
                'total_ips_tested': self.total_ips_tested,
                'sample_rate': self.args.sample_rate,
                'download_count_per_ip': self.args.dc,
                'async_ping': self.args.async_ping,
                'async_download': self.args.async_download,
            },
            'results': []
        }
        
        for result in self.results[:self.args.dn]:
            data['results'].append({
                'ip': result.ip,
                'latency_ms': round(result.latency, 2),
                'loss_rate': round(result.loss_rate, 4),
                'avg_speed_mbps': round(result.download_speed, 2),
                'all_speeds_mbps': [round(s, 2) for s in result.download_speeds] if result.download_speeds else [],
                'test_time': result.test_time
            })
        
        with open(filename, 'w', encoding='utf-8') as f:
            json.dump(data, f, indent=2, ensure_ascii=False)
    
    def run_tests(self) -> None:
        """分批执行测试"""
        print(f"{'='*60}")
        print(f"🚀 CloudflareSpeedTest {VERSION}")
        print(f"{'='*60}")
        
        ip_ranges = self.load_ip_ranges()
        print(f"📂 加载了 {len(ip_ranges)} 个IP范围")
        print(f"⚙️  最大测试数: {self.args.max_ips} 个IP")
        print(f"⚙️  批处理大小: {self.args.batch_size} IP/批")
        print(f"⚙️  采样倍率: {self.args.sample_rate}x")
        
        # 显示Ping模式
        if self.args.async_ping:
            print(f"⚙️  Ping模式: 异步（并发{self.args.async_ping_limit}）🚀")
        else:
            print(f"⚙️  Ping模式: 同步（并发{self.args.n}线程）")
        
        if self.args.dc > 1:
            print(f"⚙️  下载测试: 每IP测 {self.args.dc} 次取平均")
        print()
        
        # 阶段1: 延迟测试（分批）
        print(f"{'='*60}")
        print("🔍 阶段 1/2: 延迟测试（分批处理）")
        print(f"{'='*60}\n")
        
        ip_generator = self.generate_test_ips(ip_ranges)
        batch_num = 0
        
        while True:
            batch = []
            for _ in range(self.args.batch_size):
                try:
                    batch.append(next(ip_generator))
                except StopIteration:
                    break
            
            if not batch:
                break
            
            batch_num += 1
            print(f"📦 批次 #{batch_num}: 测试 {len(batch)} 个IP...")
            
            # 使用选定的执行器
            batch_results = self.ping_executor.execute(batch)
            
            # 合并结果
            with self.lock:
                self.results.extend(batch_results)
                for r in batch_results:
                    self.results_dict[r.ip] = r
                self.total_ips_tested += len(batch)
            
            # 排序并保留最佳结果
            self.results.sort(key=lambda x: x.latency)
            if len(self.results) > self.args.dn * 10:
                self.results = self.results[:self.args.dn * 10]
                self.results_dict = {r.ip: r for r in self.results}
            
            print(f"  ✅ 当前通过: {len(self.results)} 个IP\n")
        
        print(f"✅ 延迟测试完成: {len(self.results)} 个IP通过筛选")
        print(f"📊 共测试: {self.total_ips_tested} 个IP\n")
        
        # 阶段2: 下载测试
        if not self.args.dd and self.results:
            print(f"{'='*60}")
            print("🚀 阶段 2/2: 下载速度测试")
            print(f"{'='*60}\n")
            
            test_ips = [res.ip for res in self.results[:self.args.dn]]
            
            # 使用选定的执行器
            self.download_executor.execute(test_ips)
            
            # 按下载速度排序
            self.results.sort(key=lambda x: x.download_speed, reverse=True)
            
            print(f"\n✅ 下载测试完成!\n")
    
    def print_results(self) -> None:
        """打印测试结果"""
        print(f"{'='*60}")
        print("📊 最终测试结果")
        print(f"{'='*60}")
        
        if not self.results:
            print("❌ 没有符合条件的IP!")
            return
        
        print(f"{'IP地址':<16} {'延迟':>8} {'丢包率':>8} {'平均速度':>12}")
        print(f"{'-'*16} {'-'*8} {'-'*8} {'-'*12}")
        
        display_count = min(self.args.dn, len(self.results))
        for result in self.results[:display_count]:
            speeds_info = ""
            if len(result.download_speeds) > 1:
                speeds_str = ",".join(f"{s:.1f}" for s in result.download_speeds)
                speeds_info = f" [{speeds_str}]"
            
            print(f"{result.ip:<16} "
                  f"{result.latency:>7.2f}ms "
                  f"{result.loss_rate:>7.1%} "
                  f"{result.download_speed:>10.2f}MB/s{speeds_info}")
        
        print(f"{'='*60}")
        
        if self.results and self.results[0].download_speed > 0:
            best = self.results[0]
            print(f"\n🏆 推荐IP: {best.ip}")
            print(f"   延迟: {best.latency:.2f}ms | 平均速度: {best.download_speed:.2f}MB/s")
            if len(best.download_speeds) > 1:
                speeds_str = ", ".join(f"{s:.2f}" for s in best.download_speeds)
                print(f"   各次测速: {speeds_str} MB/s")
    
    def main(self) -> None:
        """主函数"""
        if self.args.v:
            print(f"CloudflareSpeedTest {VERSION}")
            return
        
        try:
            start_time = time.time()
            self.run_tests()
            elapsed = time.time() - start_time
            
            self.print_results()
            self.save_results()
            
            print(f"\n⏱️  总耗时: {elapsed:.1f}秒")
            print(f"{'='*60}\n")
            
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
