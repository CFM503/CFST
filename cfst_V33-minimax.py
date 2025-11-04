#!/usr/bin/env python3
"""
CloudflareSpeedTest 优化版 v3.3 - 完整修复版
- 修复socket资源泄漏问题
- 优化内存使用和并发控制
- 改进错误处理和性能监控
- 智能IP采样和流式处理
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
import psutil
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Optional, Tuple, List, Dict, Generator
from dataclasses import dataclass, field
from urllib.parse import urlparse
from contextlib import contextmanager
import warnings
from collections import defaultdict

# 第三方库
try:
    from tqdm import tqdm
    import requests
    from requests.adapters import HTTPAdapter
    from urllib3.util.retry import Retry
    from urllib3.exceptions import InsecureRequestWarning
    import urllib3
except ImportError as e:
    print(f"缺少必要的库: {e}")
    print("请运行: pip install requests tqdm urllib3 psutil")
    sys.exit(1)

warnings.filterwarnings('ignore', category=InsecureRequestWarning)
urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)

# 常量配置
DEFAULT_PORT = 443
DEFAULT_URL = "https://speed.cloudflare.com/__down?bytes=100000000"
DEFAULT_THREADS = 400
DEFAULT_TEST_COUNT = 3
DEFAULT_DOWNLOAD_COUNT = 5
DEFAULT_DOWNLOAD_SECONDS = 10
DEFAULT_IP_FILE = "ip.txt"
DEFAULT_TIMEOUT = 5
DEFAULT_MAX_LOSS_RATE = 0.0
VERSION = "v3.3"

@dataclass
class TestResult:
    """测试结果数据类"""
    ip: str
    latency: float
    loss_rate: float
    download_speed: float = 0.0
    
    def __repr__(self):
        return f"TestResult(ip={self.ip}, latency={self.latency:.2f}ms, loss={self.loss_rate:.2%}, speed={self.download_speed:.2f}MB/s)"

@dataclass
class PerformanceMetrics:
    """性能指标"""
    total_ips: int = 0
    processed_ips: int = 0
    successful_tests: int = 0
    failed_tests: int = 0
    start_time: float = field(default_factory=time.time)
    errors_by_type: Dict[str, int] = field(default_factory=lambda: defaultdict(int))
    
    def get_success_rate(self) -> float:
        if self.processed_ips == 0:
            return 0.0
        return self.successful_tests / self.processed_ips
    
    def get_ips_per_second(self) -> float:
        elapsed = time.time() - self.start_time
        return self.processed_ips / elapsed if elapsed > 0 else 0

class ErrorHandler:
    """错误处理器"""
    
    @staticmethod
    def categorize_error(exception: Exception) -> str:
        """分类错误类型"""
        error_name = type(exception).__name__
        
        if isinstance(exception, (socket.timeout, ConnectionError)):
            return "网络超时"
        elif isinstance(exception, (ConnectionRefusedError, OSError)):
            return "连接拒绝"
        elif isinstance(exception, ssl.SSLError):
            return "SSL错误"
        elif isinstance(exception, (requests.RequestException, requests.Timeout)):
            return "HTTP请求错误"
        elif isinstance(exception, MemoryError):
            return "内存不足"
        else:
            return "未知错误"
    
    @staticmethod
    def should_retry(exception: Exception, attempt: int, max_attempts: int) -> bool:
        """判断是否应该重试"""
        if attempt >= max_attempts:
            return False
        
        # 网络相关错误可以重试
        if isinstance(exception, (socket.timeout, ConnectionError, ConnectionRefusedError)):
            return True
        
        # 其他错误不重试
        return False

class SafeSocketManager:
    """安全的Socket管理器"""
    
    @contextmanager
    def create_connection(self, ip: str, port: int, timeout: int = 5):
        """创建安全的socket连接"""
        sock = None
        try:
            sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            sock.settimeout(timeout)
            sock.connect((ip, port))
            yield sock
        except Exception as e:
            print(f"连接失败: {e}")
            yield None
        finally:
            if sock:
                try:
                    sock.close()
                except:
                    pass
    
    @contextmanager
    def create_ssl_connection(self, ip: str, port: int, hostname: str, timeout: int = 5):
        """创建安全的SSL连接"""
        sock = None
        try:
            sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            sock.settimeout(timeout)
            sock.connect((ip, port))
            
            context = ssl.create_default_context()
            context.check_hostname = False
            context.verify_mode = ssl.CERT_NONE
            ssl_sock = context.wrap_socket(sock, server_hostname=hostname)
            
            yield ssl_sock
        except Exception as e:
            print(f"SSL连接失败: {e}")
            yield None
        finally:
            if sock:
                try:
                    sock.close()
                except:
                    pass

class AdaptiveConcurrency:
    """自适应并发控制器"""
    
    @staticmethod
    def calculate_optimal_workers(test_type: str, total_ips: int) -> int:
        """计算最优的并发数"""
        cpu_count = os.cpu_count() or 4
        
        if test_type == "ping":
            # Ping测试可以更高并发，因为网络I/O密集
            base_workers = min(cpu_count * 4, total_ips)
            return min(base_workers, 200)  # 限制最大并发
        elif test_type == "download":
            # 下载测试需要更多资源，并发数应该保守
            base_workers = min(cpu_count * 2, total_ips)
            return min(base_workers, 10)  # 限制最大并发
        else:
            return min(cpu_count, total_ips)
    
    @staticmethod
    def should_use_parallel_download(total_ips: int, download_time: int) -> bool:
        """决定是否使用并行下载"""
        # 如果IP数量多且单个下载时间长，建议并行
        return total_ips > 3 and download_time > 5

class MemoryEfficientIPProcessor:
    """内存高效的IP处理器"""
    
    @staticmethod
    def generate_ips_cidr(cidr: str, sample_ratio: float = 0.1) -> Generator[str, None, None]:
        """按需生成IP，避免一次性加载到内存"""
        try:
            network = ipaddress.ip_network(cidr, strict=False)
            net_size = network.num_addresses - 2
            
            if net_size <= 0:
                return
            
            # 对于大范围，使用采样
            if net_size > 1000:
                sample_size = max(1, int(net_size * sample_ratio))
                hosts = list(network.hosts())
                if len(hosts) > sample_size:
                    sampled_hosts = random.sample(hosts, sample_size)
                    for ip in sampled_hosts:
                        yield str(ip)
                else:
                    for ip in hosts:
                        yield str(ip)
            else:
                # 小范围直接生成
                for ip in network.hosts():
                    yield str(ip)
                    
        except ValueError as e:
            print(f"⚠️  跳过无效CIDR {cidr}: {e}")
    
    @staticmethod
    def validate_and_sample_ip(ip_str: str) -> Generator[str, None, None]:
        """验证并采样单个IP"""
        try:
            ipaddress.ip_address(ip_str)
            yield ip_str
        except ValueError:
            print(f"⚠️  跳过无效IP {ip_str}")

class SmartIPLoader:
    """智能IP加载器"""
    
    def __init__(self, max_memory_ips: int = 10000):
        self.max_memory_ips = max_memory_ips
    
    def load_ips_streaming(self, ip_ranges: List[str]) -> List[str]:
        """流式加载IP，避免内存溢出"""
        all_ips = []
        total_processed = 0
        
        for ip_range in ip_ranges:
            if "/" in ip_range:
                # CIDR格式
                for ip in MemoryEfficientIPProcessor.generate_ips_cidr(ip_range):
                    if len(all_ips) >= self.max_memory_ips:
                        print(f"⚠️  达到内存限制 ({self.max_memory_ips} IPs)，停止加载")
                        break
                    all_ips.append(ip)
                    total_processed += 1
            else:
                # 单个IP
                for ip in MemoryEfficientIPProcessor.validate_and_sample_ip(ip_range):
                    if len(all_ips) >= self.max_memory_ips:
                        print(f"⚠️  达到内存限制 ({self.max_memory_ips} IPs)，停止加载")
                        break
                    all_ips.append(ip)
                    total_processed += 1
            
            if len(all_ips) >= self.max_memory_ips:
                break
        
        print(f"📊 流式处理完成: 总共处理 {total_processed} 个IP，加载 {len(all_ips)} 个到内存")
        return all_ips
    
    def smart_sample_ips(self, ip_list: List[str], target_count: int) -> List[str]:
        """智能采样IP"""
        if len(ip_list) <= target_count:
            return ip_list
        
        # 按网络段分组采样
        networks = {}
        for ip in ip_list:
            try:
                ip_obj = ipaddress.ip_address(ip)
                network_key = str(ipaddress.ip_network(f"{ip}/24", strict=False))
                if network_key not in networks:
                    networks[network_key] = []
                networks[network_key].append(ip)
            except ValueError:
                continue
        
        # 从每个网络段采样
        sampled_ips = []
        ips_per_network = max(1, target_count // len(networks))
        
        for network_ips in networks.values():
            sample_size = min(ips_per_network, len(network_ips))
            sampled_ips.extend(random.sample(network_ips, sample_size))
            
            if len(sampled_ips) >= target_count:
                break
        
        # 如果还不够，随机补充
        remaining = target_count - len(sampled_ips)
        if remaining > 0:
            remaining_ips = [ip for ip in ip_list if ip not in sampled_ips]
            if remaining_ips:
                sampled_ips.extend(random.sample(remaining_ips, min(remaining, len(remaining_ips))))
        
        return sampled_ips[:target_count]

class CloudflareSpeedTest:
    def __init__(self):
        self.results: List[TestResult] = []
        self.lock = threading.Lock()
        self.ip_list: List[str] = []
        self.args = self.parse_args()
        self.valid_http_codes = set(self.args.httping_code.split(","))
        self.debug = self.args.debug
        self.socket_manager = SafeSocketManager()
        self.metrics = PerformanceMetrics()
        
        # 处理快速模式参数
        if self.args.quick:
            self.apply_quick_mode()
        
    def parse_args(self) -> argparse.Namespace:
        parser = argparse.ArgumentParser(
            description=f"Cloudflare CDN IP 测速工具 {VERSION}",
            formatter_class=argparse.RawDescriptionHelpFormatter
        )
        parser.add_argument("-n", type=int, default=DEFAULT_THREADS, 
                          help="并发线程数 (1-1000，默认400)")
        parser.add_argument("-t", type=int, default=DEFAULT_TEST_COUNT, 
                          help="每个IP的Ping测试次数 (1-10，默认3)")
        parser.add_argument("-dn", type=int, default=DEFAULT_DOWNLOAD_COUNT, 
                          help="下载测试的IP数量 (1-20，默认5)")
        parser.add_argument("-dt", type=int, default=DEFAULT_DOWNLOAD_SECONDS, 
                          help="每个IP下载测试时长/秒 (1-30，默认10)")
        parser.add_argument("-tp", type=int, default=DEFAULT_PORT, 
                          help="测试端口 (1-65535，默认443)")
        parser.add_argument("-url", default=DEFAULT_URL, 
                          help="下载测试URL")
        parser.add_argument("-httping", action="store_true", 
                          help="使用HTTP ping代替TCP ping")
        parser.add_argument("-httping-code", default="200,301,302", 
                          help="HTTP ping有效状态码，逗号分隔")
        parser.add_argument("-cfcolo", default="", 
                          help="指定Cloudflare地区代码")
        parser.add_argument("-tl", type=int, default=9999, 
                          help="最大延迟/毫秒 (默认9999)")
        parser.add_argument("-tll", type=int, default=0, 
                          help="最小延迟/毫秒 (默认0)")
        parser.add_argument("-tlr", type=float, default=DEFAULT_MAX_LOSS_RATE, 
                          help="最大丢包率 0-1 (默认0.0)")
        parser.add_argument("-sl", type=float, default=0, 
                          help="最小下载速度/MB/s (默认0)")
        parser.add_argument("-f", default=DEFAULT_IP_FILE, 
                          help="IP列表文件路径")
        parser.add_argument("-dd", action="store_true", 
                          help="禁用下载测试")
        parser.add_argument("-v", action="store_true", 
                          help="显示版本号")
        parser.add_argument("-ip", default="", 
                          help="直接指定IP，逗号分隔")
        parser.add_argument("-allip", action="store_true", 
                          help="测试所有IP（不采样）")
        parser.add_argument("--parallel-download", action="store_true",
                          help="并行下载测试（更快但更耗资源）")
        parser.add_argument("--debug", action="store_true",
                          help="显示调试信息")
        parser.add_argument("--method", choices=['requests', 'socket', 'auto'], 
                          default='auto',
                          help="下载测试方法: requests(默认)/socket(原始)/auto(自动)")
        parser.add_argument("--max-memory-ips", type=int, default=50000,
                          help="最大内存IP数量 (默认50000)")
        # 新增：时间优化参数
        parser.add_argument("--quick", action="store_true",
                          help="快速模式：自动优化参数以缩短运行时间")
        parser.add_argument("--max-ips", type=int, default=1000,
                          help="最大测试IP数量 (默认1000，超过将自动采样)")
        parser.add_argument("--sample-ratio", type=float, default=0.1,
                          help="IP采样比例 0.0-1.0 (默认0.1，即10%%)")
        parser.add_argument("--timeout", type=int, default=DEFAULT_TIMEOUT,
                          help="连接超时时间/秒 (默认5)")
        parser.add_argument("--max-retries", type=int, default=1,
                          help="最大重试次数 (默认1，设为0禁用重试)")
        parser.add_argument("--early-stop", type=int, default=0,
                          help="提前停止：找到指定数量的优质IP后停止 (默认0，不提前停止)")
        parser.add_argument("--no-ping", action="store_true",
                          help="跳过延迟测试，直接进行下载测试")
        
        # 添加帮助信息
        parser.add_argument("--help-params", action="store_true",
                          help="显示时间优化参数详细说明")
        
        args = parser.parse_args()
        
        # 参数验证
        if not (args.url.startswith("http://") or args.url.startswith("https://")):
            parser.error("URL必须以http://或https://开头")
        if not 1 <= args.n <= 1000:
            parser.error("线程数必须在1-1000之间")
        if not 1 <= args.t <= 10:
            parser.error("测试次数必须在1-10之间")
        if not 0 <= args.tlr <= 1:
            parser.error("丢包率必须在0-1之间")
        if not 0 <= args.sample_ratio <= 1:
            parser.error("采样比例必须在0-1之间")
        if not 1 <= args.timeout <= 30:
            parser.error("超时时间必须在1-30秒之间")
        if args.max_retries < 0:
            parser.error("重试次数不能为负数")
        if args.early_stop < 0:
            parser.error("提前停止数量不能为负数")
            
        return args

    def debug_print(self, msg: str):
        """调试信息打印"""
        if self.debug:
            print(f"[DEBUG] {msg}")
    
    def apply_quick_mode(self):
        """应用快速模式参数优化"""
        print("⚡ 启用快速模式，自动优化参数...")
        
        # 降低并发数，避免网络拥堵
        if self.args.n > 50:
            self.args.n = min(50, self.args.n)
            print(f"   并发数调整为: {self.args.n}")
        
        # 减少Ping测试次数
        if self.args.t > 2:
            self.args.t = 2
            print(f"   Ping测试次数调整为: {self.args.t}")
        
        # 减少下载测试时间
        if self.args.dt > 5:
            self.args.dt = 5
            print(f"   下载测试时长调整为: {self.args.dt}秒")
        
        # 减少下载测试IP数量
        if self.args.dn > 3:
            self.args.dn = 3
            print(f"   下载测试IP数调整为: {self.args.dn}")
        
        # 减少超时时间
        if self.args.timeout > 3:
            self.args.timeout = 3
            print(f"   连接超时调整为: {self.args.timeout}秒")
        
        # 禁用重试
        if self.args.max_retries > 0:
            self.args.max_retries = 0
            print(f"   重试次数调整为: {self.args.max_retries}")
        
        print()

    def load_ips(self) -> None:
        """智能加载IP列表，优化CIDR处理"""
        print(f"{'='*60}")
        print(f"🚀 CloudflareSpeedTest {VERSION}")
        print(f"{'='*60}")
        
        if self.args.ip:
            ip_ranges = [x.strip() for x in self.args.ip.split(",")]
            print(f"📍 使用命令行指定的IP: {len(ip_ranges)} 个")
        else:
            if not os.path.exists(self.args.f):
                print(f"❌ 错误: IP文件 '{self.args.f}' 不存在!")
                sys.exit(1)
            with open(self.args.f, "r", encoding='utf-8') as f:
                ip_ranges = [line.strip() for line in f if line.strip() and not line.startswith("#")]
            print(f"📂 从文件 '{self.args.f}' 加载了 {len(ip_ranges)} 个IP范围")
        
        # 使用智能加载器
        loader = SmartIPLoader(max_memory_ips=self.args.max_memory_ips)
        self.ip_list = loader.load_ips_streaming(ip_ranges)
        
        # 如果IP数量超过限制，进行智能采样
        if len(self.ip_list) > self.args.max_ips and not self.args.allip:
            print(f"⚠️  IP数量过多 ({len(self.ip_list)})，进行智能采样...")
            target_sample = min(self.args.max_ips, len(self.ip_list))
            self.ip_list = loader.smart_sample_ips(self.ip_list, target_sample)
            print(f"📊 采样后保留 {len(self.ip_list)} 个IP")
        
        # 应用采样比例（如果设置了）
        if self.args.sample_ratio < 1.0 and not self.args.allip:
            sample_size = max(1, int(len(self.ip_list) * self.args.sample_ratio))
            if sample_size < len(self.ip_list):
                print(f"📊 按比例采样: {sample_size}/{len(self.ip_list)} 个IP")
                loader = SmartIPLoader()
                self.ip_list = loader.smart_sample_ips(self.ip_list, sample_size)
        
        print(f"✅ 共加载 {len(self.ip_list)} 个待测IP\n")

    def tcp_ping(self, ip: str) -> Optional[Tuple[float, float]]:
        """TCP连接测试（优化版）"""
        success_count = 0
        total_latency = 0.0
        
        for _ in range(self.args.t):
            start = time.perf_counter()
            try:
                with self.socket_manager.create_connection((ip, self.args.tp), timeout=self.args.timeout) as sock:
                    if sock:
                        latency_ms = (time.perf_counter() - start) * 1000
                        total_latency += latency_ms
                        success_count += 1
            except Exception:
                continue
        
        if success_count == 0:
            return None
        
        avg_latency = total_latency / success_count
        loss_rate = 1 - (success_count / self.args.t)
        return avg_latency, loss_rate

    def http_ping(self, ip: str) -> Optional[Tuple[float, float]]:
        """HTTP请求测试（纯Python实现）"""
        parsed_url = urlparse(self.args.url)
        scheme = parsed_url.scheme
        hostname = parsed_url.hostname
        path = parsed_url.path or "/"
        
        url = f"{scheme}://{ip}:{self.args.tp}{path}"
        headers = {'Host': hostname}
        
        success_count = 0
        total_latency = 0.0
        
        for _ in range(self.args.t):
            start = time.perf_counter()
            try:
                response = requests.head(
                    url, 
                    headers=headers, 
                    timeout=self.args.timeout,
                    verify=False,
                    allow_redirects=False
                )
                latency_ms = (time.perf_counter() - start) * 1000
                
                if str(response.status_code) in self.valid_http_codes:
                    total_latency += latency_ms
                    success_count += 1
                    
                    if self.args.cfcolo:
                        colo = response.headers.get('CF-RAY', '').split('-')[-1]
                        if colo.upper() != self.args.cfcolo.upper():
                            return None
                            
            except Exception:
                continue
        
        if success_count == 0:
            return None
        
        avg_latency = total_latency / success_count
        loss_rate = 1 - (success_count / self.args.t)
        return avg_latency, loss_rate

    def _download_data(self, sock, start_time) -> int:
        """下载数据的通用方法"""
        downloaded = 0
        
        # 跳过HTTP头
        header_received = False
        buffer = b""
        while not header_received:
            try:
                chunk = sock.recv(4096)
                if not chunk:
                    break
                buffer += chunk
                if b"\r\n\r\n" in buffer:
                    header_end = buffer.find(b"\r\n\r\n") + 4
                    downloaded = len(buffer) - header_end
                    header_received = True
                    break
            except socket.timeout:
                break
        
        # 下载数据
        chunk_size = 32768
        while True:
            if time.perf_counter() - start_time > self.args.dt:
                break
            try:
                chunk = sock.recv(chunk_size)
                if not chunk:
                    break
                downloaded += len(chunk)
            except socket.timeout:
                break
        
        return downloaded

    def download_test_socket(self, ip: str) -> float:
        """使用原始socket下载测试（修复版）"""
        parsed_url = urlparse(self.args.url)
        hostname = parsed_url.hostname
        port = self.args.tp
        path = parsed_url.path
        if parsed_url.query:
            path += f"?{parsed_url.query}"
        
        is_https = parsed_url.scheme == 'https'
        
        try:
            downloaded = 0
            start_time = time.perf_counter()
            
            if is_https:
                with self.socket_manager.create_ssl_connection(ip, port, hostname) as ssl_sock:
                    if not ssl_sock:
                        return 0.0
                    
                    # 发送HTTP请求
                    request = (
                        f"GET {path} HTTP/1.1\r\n"
                        f"Host: {hostname}\r\n"
                        f"User-Agent: Mozilla/5.0\r\n"
                        f"Accept: */*\r\n"
                        f"Connection: close\r\n"
                        f"\r\n"
                    )
                    ssl_sock.sendall(request.encode())
                    
                    # 下载数据
                    downloaded = self._download_data(ssl_sock, start_time)
            else:
                with self.socket_manager.create_connection(ip, port) as sock:
                    if not sock:
                        return 0.0
                    
                    # 发送HTTP请求
                    request = (
                        f"GET {path} HTTP/1.1\r\n"
                        f"Host: {hostname}\r\n"
                        f"User-Agent: Mozilla/5.0\r\n"
                        f"Accept: */*\r\n"
                        f"Connection: close\r\n"
                        f"\r\n"
                    )
                    sock.sendall(request.encode())
                    
                    # 下载数据
                    downloaded = self._download_data(sock, start_time)
            
            elapsed = time.perf_counter() - start_time
            speed_mbps = (downloaded / elapsed / 1024 / 1024) if elapsed > 0 else 0
            
            self.debug_print(f"{ip}: Socket方法成功, 下载 {downloaded/1024/1024:.2f}MB, 速度 {speed_mbps:.2f}MB/s")
            return speed_mbps
            
        except Exception as e:
            self.debug_print(f"{ip}: Socket方法失败: {e}")
            return 0.0

    def download_test_requests(self, ip: str) -> float:
        """使用requests库下载测试"""
        parsed_url = urlparse(self.args.url)
        scheme = parsed_url.scheme
        hostname = parsed_url.hostname
        
        # 方法1: 直接用IP构建URL
        url = f"{scheme}://{ip}:{self.args.tp}{parsed_url.path}"
        if parsed_url.query:
            url += f"?{parsed_url.query}"
        
        headers = {
            'Host': hostname,
            'User-Agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36',
            'Accept': '*/*',
            'Connection': 'keep-alive',
        }
        
        try:
            start_time = time.perf_counter()
            downloaded = 0
            
            # 创建session
            session = requests.Session()
            session.verify = False
            session.headers.update(headers)
            
            with session.get(url, stream=True, timeout=self.args.timeout) as response:
                if response.status_code not in [200, 206]:
                    self.debug_print(f"{ip}: HTTP状态码 {response.status_code}")
                    return 0.0
                
                for chunk in response.iter_content(chunk_size=32768):
                    if chunk:
                        downloaded += len(chunk)
                    
                    if time.perf_counter() - start_time > self.args.dt:
                        break
            
            elapsed = time.perf_counter() - start_time
            speed_mbps = (downloaded / elapsed / 1024 / 1024) if elapsed > 0 else 0
            
            self.debug_print(f"{ip}: Requests方法成功, 下载 {downloaded/1024/1024:.2f}MB, 速度 {speed_mbps:.2f}MB/s")
            return speed_mbps
            
        except Exception as e:
            self.debug_print(f"{ip}: Requests方法失败: {e}")
            return 0.0

    def download_test(self, ip: str) -> float:
        """下载速度测试（自动选择最佳方法）"""
        if self.args.method == 'socket':
            return self.download_test_socket(ip)
        elif self.args.method == 'requests':
            return self.download_test_requests(ip)
        else:  # auto
            # 先尝试requests，失败则用socket
            speed = self.download_test_requests(ip)
            if speed == 0:
                self.debug_print(f"{ip}: Requests失败，切换到Socket方法")
                speed = self.download_test_socket(ip)
            return speed

    def worker_ping(self, ip: str) -> None:
        """延迟测试工作线程（改进版）"""
        max_attempts = max(1, self.args.max_retries + 1)
        for attempt in range(max_attempts):
            try:
                result = self.http_ping(ip) if self.args.httping else self.tcp_ping(ip)
                
                if result:
                    latency, loss_rate = result
                    if (self.args.tll <= latency <= self.args.tl and 
                        loss_rate <= self.args.tlr):
                        with self.lock:
                            self.results.append(TestResult(ip, latency, loss_rate))
                        self.metrics.successful_tests += 1
                        
                        # 检查是否需要提前停止
                        if self.args.early_stop > 0 and len(self.results) >= self.args.early_stop:
                            return
                        return
                else:
                    # 测试失败，但可能是正常的（IP不可达）
                    self.metrics.failed_tests += 1
                    return
                    
            except Exception as e:
                error_type = ErrorHandler.categorize_error(e)
                
                if self.args.max_retries > 0 and ErrorHandler.should_retry(e, attempt, max_attempts):
                    time.sleep(0.1 * (attempt + 1))  # 指数退避
                    continue
                else:
                    self.debug_print(f"{ip}: 最终失败 ({error_type}): {e}")
                    self.metrics.failed_tests += 1
                    self.metrics.errors_by_type[error_type] += 1
                    return
            
            self.metrics.processed_ips += 1

    def worker_download(self, ip: str) -> None:
        """下载测试工作线程（改进版）"""
        max_attempts = max(1, self.args.max_retries + 1)
        for attempt in range(max_attempts):
            try:
                speed = self.download_test(ip)
                
                if speed > 0:
                    with self.lock:
                        for result in self.results:
                            if result.ip == ip:
                                result.download_speed = speed
                                break
                    self.metrics.successful_tests += 1
                    return
                else:
                    # 下载失败
                    self.metrics.failed_tests += 1
                    return
                    
            except Exception as e:
                error_type = ErrorHandler.categorize_error(e)
                
                if self.args.max_retries > 0 and ErrorHandler.should_retry(e, attempt, max_attempts):
                    time.sleep(0.2 * (attempt + 1))
                    continue
                else:
                    self.debug_print(f"{ip}: 下载最终失败 ({error_type}): {e}")
                    self.metrics.failed_tests += 1
                    self.metrics.errors_by_type[error_type] += 1
                    return
            
            self.metrics.processed_ips += 1

    def run_tests(self) -> None:
        """执行所有测试（改进版）"""
        
        # 跳过延迟测试模式
        if self.args.no_ping:
            print(f"{'='*60}")
            print("🚀 直接下载测试模式")
            print(f"{'='*60}")
            print("⚠️  跳过延迟测试，直接进行下载测试")
            
            # 为所有IP创建默认结果（延迟设为0）
            for ip in self.ip_list[:self.args.dn]:
                self.results.append(TestResult(ip, 0.0, 0.0))
            
            test_ips = [res.ip for res in self.results]
        else:
            # 阶段1: 延迟测试
            print(f"{'='*60}")
            print("🔍 阶段 1/2: 延迟测试")
            print(f"{'='*60}")
            print(f"⚙️  测试模式: {'HTTP Ping' if self.args.httping else 'TCP Ping'}")
            print(f"⚙️  并发线程: {self.args.n}")
            print(f"⚙️  测试次数: {self.args.t}")
            print(f"⚙️  过滤条件: 延迟 {self.args.tll}-{self.args.tl}ms, 丢包率 ≤{self.args.tlr*100}%")
            if self.args.early_stop > 0:
                print(f"⚙️  提前停止: 找到 {self.args.early_stop} 个优质IP后停止")
            print()
            
            # 使用自适应并发
            optimal_workers = AdaptiveConcurrency.calculate_optimal_workers("ping", len(self.ip_list))
            actual_workers = min(self.args.n, optimal_workers)
            
            print(f"⚡ 实际并发数: {actual_workers} (系统推荐: {optimal_workers})")
            
            with ThreadPoolExecutor(max_workers=actual_workers) as executor:
                list(tqdm(
                    executor.map(self.worker_ping, self.ip_list),
                    total=len(self.ip_list),
                    desc="⏱️  Ping测试",
                    ncols=80,
                    unit="IP"
                ))
            
            self.results.sort(key=lambda x: x.latency)
            
            print(f"\n✅ 延迟测试完成: {len(self.results)}/{len(self.ip_list)} 个IP通过筛选\n")
            
            if not self.results:
                print("❌ 没有IP通过延迟测试，无法进行下载测试!")
                return
            
            test_ips = [res.ip for res in self.results[:self.args.dn]]
        
        # 阶段2: 下载测试
        if not self.args.dd and test_ips:
            print(f"{'='*60}")
            print("🚀 阶段 2/2: 下载速度测试")
            print(f"{'='*60}")
            
            print(f"⚙️  测试IP数: {len(test_ips)}")
            print(f"⚙️  测试时长: {self.args.dt}秒/IP")
            print(f"⚙️  测试方法: {self.args.method}")
            
            # 智能决定是否并行
            if not self.args.parallel_download:
                should_parallel = AdaptiveConcurrency.should_use_parallel_download(
                    len(test_ips), self.args.dt
                )
                parallel_mode = "并行" if should_parallel else "串行"
                print(f"⚙️  测试模式: {parallel_mode} (自动选择)")
            else:
                parallel_mode = "并行"
                print(f"⚙️  测试模式: {parallel_mode} (用户指定)")
            
            # 使用自适应并发
            optimal_workers = AdaptiveConcurrency.calculate_optimal_workers("download", len(test_ips))
            
            if parallel_mode == "并行":
                actual_workers = min(optimal_workers, len(test_ips))
                print(f"⚡ 实际并发数: {actual_workers} (系统推荐: {optimal_workers})")
                
                with ThreadPoolExecutor(max_workers=actual_workers) as executor:
                    futures = [executor.submit(self.worker_download, ip) for ip in test_ips]
                    for future in tqdm(as_completed(futures), total=len(test_ips), desc="🚀 下载测试", ncols=80):
                        pass
            else:
                print(f"⚡ 使用串行模式")
                for ip in test_ips:
                    print(f"\n测试 {ip}...")
                    self.worker_download(ip)
            
            self.results.sort(key=lambda x: x.download_speed, reverse=True)
            print(f"\n✅ 下载测试完成!\n")

    def print_results(self) -> None:
        """打印测试结果"""
        print(f"{'='*60}")
        print("📊 测试结果")
        print(f"{'='*60}")
        
        if not self.results:
            print("❌ 没有符合条件的IP!")
            return
        
        print(f"{'IP地址':<16} {'延迟':>8} {'丢包率':>8} {'下载速度':>12}")
        print(f"{'-'*16} {'-'*8} {'-'*8} {'-'*12}")
        
        display_count = min(self.args.dn, len(self.results))
        for result in self.results[:display_count]:
            print(f"{result.ip:<16} "
                  f"{result.latency:>7.2f}ms "
                  f"{result.loss_rate:>7.1%} "
                  f"{result.download_speed:>10.2f}MB/s")
        
        print(f"{'='*60}")
        
        if self.results and self.results[0].download_speed > 0:
            best = self.results[0]
            print(f"\n🏆 推荐IP: {best.ip}")
            print(f"   延迟: {best.latency:.2f}ms | 速度: {best.download_speed:.2f}MB/s")
        
        # 打印性能统计
        self.print_performance_summary()

    def print_performance_summary(self):
        """打印性能统计"""
        print(f"\n📊 性能统计:")
        print(f"   总IP数: {self.metrics.total_ips}")
        print(f"   处理IP: {self.metrics.processed_ips}")
        print(f"   成功率: {self.metrics.get_success_rate():.1%}")
        print(f"   处理速度: {self.metrics.get_ips_per_second():.1f} IP/秒")
        
        if self.metrics.errors_by_type:
            print(f"   错误统计:")
            for error_type, count in self.metrics.errors_by_type.items():
                print(f"     {error_type}: {count}")

    def print_time_optimization_help(self):
        """打印时间优化参数帮助"""
        print(f"{'='*80}")
        print("⏱️  时间优化参数详细说明")
        print(f"{'='*80}")
        print()
        print("🚀 快速模式:")
        print("   --quick           自动优化所有参数以缩短运行时间")
        print()
        print("📊 IP数量控制:")
        print("   --max-ips N       最大测试IP数量 (默认1000)")
        print("   --sample-ratio R  IP采样比例 0.0-1.0 (默认0.1，即10%)")
        print("   -allip           测试所有IP（不采样）")
        print()
        print("⚡ 性能优化:")
        print("   -n N             并发线程数 (默认400，建议10-50)")
        print("   -t N             每个IP的Ping测试次数 (默认3，建议1-2)")
        print("   --timeout S      连接超时时间/秒 (默认5，建议1-3)")
        print("   --max-retries N  最大重试次数 (默认1，建议0)")
        print()
        print("🎯 提前停止:")
        print("   --early-stop N   找到N个优质IP后停止 (默认0，不提前停止)")
        print()
        print("⚡ 跳过测试:")
        print("   --no-ping        跳过延迟测试，直接进行下载测试")
        print("   -dd              禁用下载测试")
        print()
        print("🚀 下载测试优化:")
        print("   -dn N            下载测试的IP数量 (默认5，建议1-3)")
        print("   -dt S            每个IP下载测试时长/秒 (默认10，建议3-5)")
        print()
        print("💡 建议的时间优化组合:")
        print("   快速测试: --quick")
        print("   超快测试: --quick --early-stop 5 --no-ping")
        print("   平衡测试: -n 20 -t 2 -dt 3 -dn 3")
        print(f"{'='*80}")
    
    def main(self) -> None:
        """主函数"""
        if self.args.v:
            print(f"CloudflareSpeedTest {VERSION}")
            return
        
        if self.args.help_params:
            self.print_time_optimization_help()
            return
        
        try:
            self.load_ips()
            
            if not self.ip_list:
                print("❌ 错误: 没有有效的IP地址!")
                return
            
            self.metrics.total_ips = len(self.ip_list)
            
            start_time = time.time()
            self.run_tests()
            elapsed = time.time() - start_time
            
            self.print_results()
            print(f"\n⏱️  总耗时: {elapsed:.1f}秒")
            print(f"{'='*60}\n")
            
        except KeyboardInterrupt:
            print("\n\n⚠️  程序被用户中断!")
            self.print_performance_summary()
            sys.exit(0)
        except Exception as e:
            print(f"\n❌ 意外错误: {e}")
            import traceback
            traceback.print_exc()
            self.print_performance_summary()
            sys.exit(1)


# v3.3 (2025-11-02)
# 演示版本号更新功能

# v3.3 (2025-11-02)
# 添加演示更新

if __name__ == "__main__":
    tester = CloudflareSpeedTest()
    tester.main()