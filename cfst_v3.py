#!/usr/bin/env python3
"""
CloudflareSpeedTest 优化版 v3.1 - 修复下载测试问题
- 添加详细调试信息
- 多种下载方案自动切换
- 更好的错误处理
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
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Optional, Tuple, List, Dict
from dataclasses import dataclass
from urllib.parse import urlparse
import warnings

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
    print("请运行: pip install requests tqdm urllib3")
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
VERSION = "v3.1-fixed"

@dataclass
class TestResult:
    """测试结果数据类"""
    ip: str
    latency: float
    loss_rate: float
    download_speed: float = 0.0
    
    def __repr__(self):
        return f"TestResult(ip={self.ip}, latency={self.latency:.2f}ms, loss={self.loss_rate:.2%}, speed={self.download_speed:.2f}MB/s)"


class CloudflareSpeedTest:
    def __init__(self):
        self.results: List[TestResult] = []
        self.lock = threading.Lock()
        self.ip_list: List[str] = []
        self.args = self.parse_args()
        self.valid_http_codes = set(self.args.httping_code.split(","))
        self.debug = self.args.debug
        
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
            
        return args

    def debug_print(self, msg: str):
        """调试信息打印"""
        if self.debug:
            print(f"[DEBUG] {msg}")

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
        
        total_ips = 0
        for ip_range in tqdm(ip_ranges, desc="⚙️  解析IP范围", ncols=80):
            if "/" in ip_range:
                try:
                    network = ipaddress.ip_network(ip_range, strict=False)
                    net_size = network.num_addresses - 2
                    
                    if self.args.allip:
                        if net_size > 10000:
                            print(f"⚠️  警告: {ip_range} 包含 {net_size} 个IP，这会很慢！")
                        self.ip_list.extend(str(ip) for ip in network.hosts())
                        total_ips += net_size
                    else:
                        # 智能采样
                        if network.prefixlen <= 16:
                            sample_count = min(256, net_size // 256)
                            subnets = list(network.subnets(new_prefix=24))
                            for subnet in random.sample(subnets, min(len(subnets), sample_count)):
                                hosts = list(subnet.hosts())
                                if hosts:
                                    self.ip_list.append(str(random.choice(hosts)))
                            total_ips += sample_count
                        elif network.prefixlen <= 24:
                            hosts = list(network.hosts())
                            sample_count = min(10, len(hosts))
                            self.ip_list.extend(str(ip) for ip in random.sample(hosts, sample_count))
                            total_ips += sample_count
                        else:
                            hosts = list(network.hosts())
                            if hosts:
                                self.ip_list.append(str(random.choice(hosts)))
                                total_ips += 1
                                
                except ValueError as e:
                    print(f"⚠️  跳过无效CIDR {ip_range}: {e}")
            else:
                try:
                    ipaddress.ip_address(ip_range)
                    self.ip_list.append(ip_range)
                    total_ips += 1
                except ValueError:
                    print(f"⚠️  跳过无效IP {ip_range}")
        
        print(f"✅ 共加载 {len(self.ip_list)} 个待测IP\n")

    def tcp_ping(self, ip: str) -> Optional[Tuple[float, float]]:
        """TCP连接测试（优化版）"""
        success_count = 0
        total_latency = 0.0
        
        for _ in range(self.args.t):
            start = time.perf_counter()
            try:
                with socket.create_connection((ip, self.args.tp), timeout=DEFAULT_TIMEOUT):
                    latency_ms = (time.perf_counter() - start) * 1000
                    total_latency += latency_ms
                    success_count += 1
            except (socket.timeout, socket.error, OSError):
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
                    timeout=DEFAULT_TIMEOUT,
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
                            
            except (requests.RequestException, OSError):
                continue
        
        if success_count == 0:
            return None
        
        avg_latency = total_latency / success_count
        loss_rate = 1 - (success_count / self.args.t)
        return avg_latency, loss_rate

    def download_test_socket(self, ip: str) -> float:
        """使用原始socket下载测试（最可靠的方法）"""
        parsed_url = urlparse(self.args.url)
        hostname = parsed_url.hostname
        port = self.args.tp
        path = parsed_url.path
        if parsed_url.query:
            path += f"?{parsed_url.query}"
        
        is_https = parsed_url.scheme == 'https'
        
        try:
            # 创建socket连接
            sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            sock.settimeout(DEFAULT_TIMEOUT)
            sock.connect((ip, port))
            
            # HTTPS需要SSL包装
            if is_https:
                context = ssl.create_default_context()
                context.check_hostname = False
                context.verify_mode = ssl.CERT_NONE
                sock = context.wrap_socket(sock, server_hostname=hostname)
            
            # 发送HTTP GET请求
            request = (
                f"GET {path} HTTP/1.1\r\n"
                f"Host: {hostname}\r\n"
                f"User-Agent: Mozilla/5.0\r\n"
                f"Accept: */*\r\n"
                f"Connection: close\r\n"
                f"\r\n"
            )
            sock.sendall(request.encode())
            
            # 接收响应
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
            
            sock.close()
            
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
            
            with session.get(url, stream=True, timeout=DEFAULT_TIMEOUT) as response:
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
        """延迟测试工作线程"""
        result = self.http_ping(ip) if self.args.httping else self.tcp_ping(ip)
        
        if result:
            latency, loss_rate = result
            if (self.args.tll <= latency <= self.args.tl and 
                loss_rate <= self.args.tlr):
                with self.lock:
                    self.results.append(TestResult(ip, latency, loss_rate))

    def worker_download(self, ip: str) -> None:
        """下载测试工作线程"""
        speed = self.download_test(ip)
        
        if speed >= self.args.sl:
            with self.lock:
                for result in self.results:
                    if result.ip == ip:
                        result.download_speed = speed
                        break

    def run_tests(self) -> None:
        """执行所有测试"""
        # 阶段1: 延迟测试
        print(f"{'='*60}")
        print("🔍 阶段 1/2: 延迟测试")
        print(f"{'='*60}")
        print(f"⚙️  测试模式: {'HTTP Ping' if self.args.httping else 'TCP Ping'}")
        print(f"⚙️  并发线程: {self.args.n}")
        print(f"⚙️  测试次数: {self.args.t}")
        print(f"⚙️  过滤条件: 延迟 {self.args.tll}-{self.args.tl}ms, 丢包率 ≤{self.args.tlr*100}%\n")
        
        with ThreadPoolExecutor(max_workers=self.args.n) as executor:
            list(tqdm(
                executor.map(self.worker_ping, self.ip_list),
                total=len(self.ip_list),
                desc="⏱️  Ping测试",
                ncols=80,
                unit="IP"
            ))
        
        self.results.sort(key=lambda x: x.latency)
        
        print(f"\n✅ 延迟测试完成: {len(self.results)}/{len(self.ip_list)} 个IP通过筛选\n")
        
        # 阶段2: 下载测试
        if not self.args.dd and self.results:
            print(f"{'='*60}")
            print("🚀 阶段 2/2: 下载速度测试")
            print(f"{'='*60}")
            
            test_ips = [res.ip for res in self.results[:self.args.dn]]
            print(f"⚙️  测试IP数: {len(test_ips)}")
            print(f"⚙️  测试时长: {self.args.dt}秒/IP")
            print(f"⚙️  测试方法: {self.args.method}")
            print(f"⚙️  测试模式: {'并行' if self.args.parallel_download else '串行'}\n")
            
            if self.args.parallel_download:
                with ThreadPoolExecutor(max_workers=min(len(test_ips), 5)) as executor:
                    futures = [executor.submit(self.worker_download, ip) for ip in test_ips]
                    for future in tqdm(as_completed(futures), total=len(test_ips), desc="🚀 下载测试", ncols=80):
                        pass
            else:
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

    def main(self) -> None:
        """主函数"""
        if self.args.v:
            print(f"CloudflareSpeedTest {VERSION}")
            return
        
        try:
            self.load_ips()
            
            if not self.ip_list:
                print("❌ 错误: 没有有效的IP地址!")
                return
            
            start_time = time.time()
            self.run_tests()
            elapsed = time.time() - start_time
            
            self.print_results()
            print(f"\n⏱️  总耗时: {elapsed:.1f}秒")
            print(f"{'='*60}\n")
            
        except KeyboardInterrupt:
            print("\n\n⚠️  程序被用户中断!")
            sys.exit(0)
        except Exception as e:
            print(f"\n❌ 意外错误: {e}")
            import traceback
            traceback.print_exc()
            sys.exit(1)


if __name__ == "__main__":
    tester = CloudflareSpeedTest()
    tester.main()