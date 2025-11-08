#!/usr/bin/env python3
"""
CloudflareSpeedTest 增强版 v3.3
新功能：
1. 结果保存到CSV/JSON文件
3. 智能采样倍率可调
4. 多次下载测速取平均
5. 实时最佳IP显示
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
from concurrent.futures import ThreadPoolExecutor
from typing import Optional, Tuple, List, Iterator
from dataclasses import dataclass, asdict
from urllib.parse import urlparse
from datetime import datetime
import warnings

try:
    from tqdm import tqdm
    import requests
    import urllib3
except ImportError as e:
    print(f"缺少必要的库: {e}")
    print("请运行: pip install requests tqdm urllib3")
    sys.exit(1)

warnings.filterwarnings('ignore')
urllib3.disable_warnings()

# 常量配置
DEFAULT_PORT = 443
DEFAULT_URL = "https://speed.cloudflare.com/__down?bytes=1000000000"
DEFAULT_THREADS = 400
DEFAULT_TEST_COUNT = 3
DEFAULT_DOWNLOAD_COUNT = 5
DEFAULT_DOWNLOAD_SECONDS = 10
DEFAULT_IP_FILE = "ip.txt"
DEFAULT_TIMEOUT = 5
DEFAULT_MAX_LOSS_RATE = 0.0
VERSION = "v3.3-enhanced"

# 内存优化参数
MAX_IP_BATCH = 10000
CIDR_SAMPLE_LIMIT = 1000

@dataclass
class TestResult:
    """测试结果数据类"""
    ip: str
    latency: float
    loss_rate: float
    download_speed: float = 0.0
    download_speeds: List[float] = None  # 多次测速结果
    test_time: str = ""
    
    def __post_init__(self):
        if self.download_speeds is None:
            self.download_speeds = []
        if not self.test_time:
            self.test_time = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
    
    def __repr__(self):
        return f"{self.ip}: {self.latency:.2f}ms, {self.loss_rate:.1%}, {self.download_speed:.2f}MB/s"


class CloudflareSpeedTest:
    def __init__(self):
        self.results: List[TestResult] = []
        self.lock = threading.Lock()
        self.args = self.parse_args()
        self.valid_http_codes = set(self.args.httping_code.split(","))
        self.debug = self.args.debug
        self.total_ips_tested = 0
        self.best_ips_display = []  # 实时最佳IP列表
        self.sample_multiplier = self.args.sample_rate  # 采样倍率
        
    def parse_args(self) -> argparse.Namespace:
        parser = argparse.ArgumentParser(
            description=f"Cloudflare CDN IP 测速工具 {VERSION}",
            formatter_class=argparse.RawDescriptionHelpFormatter,
            epilog="""
示例:
  基础测试:
    python3 %(prog)s -n 100 --max-ips 10000 -dn 5
  
  增加采样数量:
    python3 %(prog)s --sample-rate 2.0 --max-ips 20000
  
  多次测速取平均:
    python3 %(prog)s -dn 5 -dc 3
  
  保存结果到文件:
    python3 %(prog)s -o result.csv
    python3 %(prog)s -o result.json
            """
        )
        parser.add_argument("-n", type=int, default=200, 
                          help="并发线程数 (1-500，默认200)")
        parser.add_argument("-t", type=int, default=DEFAULT_TEST_COUNT, 
                          help="每个IP的Ping测试次数 (1-10，默认3)")
        parser.add_argument("-dn", type=int, default=DEFAULT_DOWNLOAD_COUNT, 
                          help="下载测试的IP数量 (1-20，默认5)")
        parser.add_argument("-dt", type=int, default=DEFAULT_DOWNLOAD_SECONDS, 
                          help="每个IP下载测试时长/秒 (1-30，默认10)")
        parser.add_argument("-dc", type=int, default=1,
                          help="每个IP下载测试次数，取平均值 (1-5，默认1)")
        parser.add_argument("-tp", type=int, default=DEFAULT_PORT, 
                          help="测试端口 (默认443)")
        parser.add_argument("-url", default=DEFAULT_URL, 
                          help="下载测试URL")
        parser.add_argument("-httping", action="store_true", 
                          help="使用HTTP ping代替TCP ping")
        parser.add_argument("-httping-code", default="200,301,302", 
                          help="HTTP ping有效状态码")
        parser.add_argument("-cfcolo", default="", 
                          help="指定Cloudflare地区代码")
        parser.add_argument("-tl", type=int, default=9999, 
                          help="最大延迟/毫秒")
        parser.add_argument("-tll", type=int, default=0, 
                          help="最小延迟/毫秒")
        parser.add_argument("-tlr", type=float, default=DEFAULT_MAX_LOSS_RATE, 
                          help="最大丢包率 0-1")
        parser.add_argument("-sl", type=float, default=0, 
                          help="最小下载速度/MB/s")
        parser.add_argument("-f", default=DEFAULT_IP_FILE, 
                          help="IP列表文件路径")
        parser.add_argument("-o", "--output", default="",
                          help="保存结果到文件 (支持.csv/.json)")
        parser.add_argument("-dd", action="store_true", 
                          help="禁用下载测试")
        parser.add_argument("-v", action="store_true", 
                          help="显示版本号")
        parser.add_argument("-ip", default="", 
                          help="直接指定IP，逗号分隔")
        parser.add_argument("-allip", action="store_true", 
                          help="测试所有IP（警告：内存消耗大）")
        parser.add_argument("--debug", action="store_true",
                          help="显示调试信息")
        parser.add_argument("--method", choices=['requests', 'socket', 'auto'], 
                          default='socket',
                          help="下载方法: socket(默认)/requests/auto")
        parser.add_argument("--max-ips", type=int, default=50000,
                          help="最多测试的IP总数（默认50000）")
        parser.add_argument("--batch-size", type=int, default=5000,
                          help="每批处理的IP数量（默认5000）")
        parser.add_argument("--sample-rate", type=float, default=1.0,
                          help="采样倍率 (0.5-5.0，默认1.0，越大测试IP越多)")
        parser.add_argument("--show-progress", action="store_true",
                          help="显示实时最佳IP排行（默认开启）")
        
        args = parser.parse_args()
        
        # 参数验证
        if args.dc < 1 or args.dc > 5:
            parser.error("下载测试次数必须在1-5之间")
        if args.sample_rate < 0.5 or args.sample_rate > 5.0:
            parser.error("采样倍率必须在0.5-5.0之间")
        
        return args

    def debug_print(self, msg: str):
        """调试信息打印"""
        if self.debug:
            print(f"[DEBUG] {msg}")

    def update_best_ips_display(self):
        """更新实时最佳IP显示"""
        if not self.args.show_progress and not self.debug:
            return
        
        # 获取当前最佳的3个IP
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

    def sample_ips_from_cidr(self, cidr: str, max_samples: int) -> Iterator[str]:
        """从CIDR范围生成IP样本（生成器模式）- 支持采样倍率"""
        try:
            network = ipaddress.ip_network(cidr, strict=False)
            total_hosts = network.num_addresses - 2
            
            if total_hosts <= 0:
                return
            
            # 应用采样倍率
            max_samples = int(max_samples * self.sample_multiplier)
            
            if self.args.allip:
                actual_samples = min(max_samples, total_hosts)
                self.debug_print(f"{cidr}: 全测试模式，采样 {actual_samples}/{total_hosts} 个IP")
                
                hosts = list(network.hosts())
                if len(hosts) <= max_samples:
                    for ip in hosts:
                        yield str(ip)
                else:
                    for ip in random.sample(list(hosts), max_samples):
                        yield str(ip)
            else:
                # 智能采样模式（应用倍率）
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
        """加载IP范围列表（不解析）"""
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
        """TCP连接测试"""
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
        """HTTP请求测试"""
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
                    url, headers=headers, timeout=DEFAULT_TIMEOUT,
                    verify=False, allow_redirects=False
                )
                latency_ms = (time.perf_counter() - start) * 1000
                
                if str(response.status_code) in self.valid_http_codes:
                    total_latency += latency_ms
                    success_count += 1
                    
                    if self.args.cfcolo:
                        colo = response.headers.get('CF-RAY', '').split('-')[-1]
                        if colo.upper() != self.args.cfcolo.upper():
                            return None
            except:
                continue
        
        if success_count == 0:
            return None
        
        avg_latency = total_latency / success_count
        loss_rate = 1 - (success_count / self.args.t)
        return avg_latency, loss_rate

    def download_test_socket(self, ip: str) -> float:
        """使用原始socket下载测试"""
        parsed_url = urlparse(self.args.url)
        hostname = parsed_url.hostname
        port = self.args.tp
        path = parsed_url.path
        if parsed_url.query:
            path += f"?{parsed_url.query}"
        
        is_https = parsed_url.scheme == 'https'
        
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
                except:
                    break
            
            sock.close()
            
            elapsed = time.perf_counter() - start_time
            speed_mbps = (downloaded / elapsed / 1024 / 1024) if elapsed > 0 else 0
            return speed_mbps
            
        except Exception as e:
            self.debug_print(f"{ip}: Socket下载失败: {e}")
            return 0.0

    def download_test(self, ip: str, test_num: int = 1) -> float:
        """下载速度测试 - 支持多次测试"""
        speeds = []
        
        for i in range(self.args.dc):
            if self.args.dc > 1:
                print(f"    第 {i+1}/{self.args.dc} 次测试...", end=" ")
            
            speed = self.download_test_socket(ip)
            speeds.append(speed)
            
            if self.args.dc > 1:
                print(f"{speed:.2f} MB/s")
            
            # 如果第一次就失败，不再继续
            if speed == 0 and i == 0:
                break
            
            # 多次测试之间短暂延迟
            if i < self.args.dc - 1 and speed > 0:
                time.sleep(0.5)
        
        # 过滤掉0值，计算平均
        valid_speeds = [s for s in speeds if s > 0]
        if not valid_speeds:
            return 0.0
        
        avg_speed = sum(valid_speeds) / len(valid_speeds)
        
        # 保存多次测试结果
        with self.lock:
            for result in self.results:
                if result.ip == ip:
                    result.download_speeds = speeds
                    break
        
        return avg_speed

    def worker_ping(self, ip: str) -> None:
        """延迟测试工作线程"""
        result = self.http_ping(ip) if self.args.httping else self.tcp_ping(ip)
        
        if result:
            latency, loss_rate = result
            if (self.args.tll <= latency <= self.args.tl and 
                loss_rate <= self.args.tlr):
                with self.lock:
                    self.results.append(TestResult(ip, latency, loss_rate))
                    self.total_ips_tested += 1

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
                # 默认CSV
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
            # 写入表头
            writer.writerow([
                'IP地址', '延迟(ms)', '丢包率(%)', '平均速度(MB/s)', 
                '测试次数', '各次速度(MB/s)', '测试时间'
            ])
            
            # 写入数据
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
        print(f"⚙️  并发线程: {self.args.n}")
        print(f"⚙️  采样倍率: {self.args.sample_rate}x")
        print(f"⚙️  测试模式: {'HTTP Ping' if self.args.httping else 'TCP Ping'}")
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
            
            with ThreadPoolExecutor(max_workers=self.args.n) as executor:
                list(tqdm(
                    executor.map(self.worker_ping, batch),
                    total=len(batch),
                    desc=f"  ⏱️  Ping",
                    ncols=80,
                    leave=False
                ))
            
            self.results.sort(key=lambda x: x.latency)
            if len(self.results) > self.args.dn * 10:
                self.results = self.results[:self.args.dn * 10]
            
            print(f"  ✅ 当前通过: {len(self.results)} 个IP\n")
        
        print(f"✅ 延迟测试完成: {len(self.results)} 个IP通过筛选")
        print(f"📊 共测试: {self.total_ips_tested} 个IP\n")
        
        # 阶段2: 下载测试
        if not self.args.dd and self.results:
            print(f"{'='*60}")
            print("🚀 阶段 2/2: 下载速度测试")
            print(f"{'='*60}\n")
            
            test_ips = [res.ip for res in self.results[:self.args.dn]]
            
            for i, ip in enumerate(test_ips, 1):
                print(f"[{i}/{len(test_ips)}] 测试 {ip}...")
                speed = self.download_test(ip, i)
                
                with self.lock:
                    for result in self.results:
                        if result.ip == ip:
                            result.download_speed = speed
                            break
                
                if speed > 0:
                    if self.args.dc > 1:
                        print(f"  ✅ 平均速度: {speed:.2f} MB/s\n")
                    else:
                        print(f"  ✅ 速度: {speed:.2f} MB/s\n")
                else:
                    print(f"  ❌ 下载失败\n")
                
                # 显示实时最佳IP
                self.update_best_ips_display()
            
            self.results.sort(key=lambda x: x.download_speed, reverse=True)

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
            
            # 保存结果
            self.save_results()
            
            print(f"\n⏱️  总耗时: {elapsed:.1f}秒")
            print(f"{'='*60}\n")
            
        except KeyboardInterrupt:
            print("\n\n⚠️  程序被用户中断!")
            # 中断时也尝试保存已有结果
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
