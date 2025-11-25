#!/usr/bin/env python3
"""
CloudflareSpeedTest v3.8c - 三维评分系统
核心功能：
- 三维评分：速度 + 稳定性 + 延迟
- 实时下载进度显示
- 完整的延迟信息展示
- 可调权重参数
"""
import os, sys, time, random, socket, ipaddress, threading
import argparse, ssl, csv, json, asyncio, math, statistics, warnings
from abc import ABC, abstractmethod
from concurrent.futures import ThreadPoolExecutor
from typing import Optional, Tuple, List, Iterator, Dict
from dataclasses import dataclass, field
from urllib.parse import urlparse
from datetime import datetime

try:
    from tqdm import tqdm
    import requests, urllib3
except ImportError as e:
    print(f"缺少库: {e}\n请运行: pip install requests tqdm urllib3")
    sys.exit(1)

ASYNC_AVAILABLE = sys.version_info >= (3, 7)
warnings.filterwarnings('ignore')
urllib3.disable_warnings()

VERSION = "v3.8c"
DEFAULT_PORT = 443
DEFAULT_URL = "https://speed.cloudflare.com/__down?bytes=1000000000"
DEFAULT_THREADS = 200
DEFAULT_TEST_COUNT = 3
DEFAULT_DOWNLOAD_COUNT = 10
DEFAULT_DOWNLOAD_SECONDS = 10
DEFAULT_IP_FILE = "ip.txt"
DEFAULT_TIMEOUT = 5
DEFAULT_ASYNC_LIMIT = 512

@dataclass
class TestResult:
    ip: str
    latency: float
    loss_rate: float
    download_speed: float = 0.0
    download_speeds: List[float] = field(default_factory=list)
    test_time: str = ""
    speed_std: float = 0.0
    speed_min: float = 0.0
    speed_max: float = 0.0
    speed_cv: float = 0.0
    stability_score: float = 0.0
    speed_score: float = 0.0
    latency_score: float = 0.0
    composite_score: float = 0.0
    
    def __post_init__(self):
        if not self.test_time:
            self.test_time = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
    
    def calculate_stability_metrics(self):
        if not self.download_speeds or len(self.download_speeds) < 2:
            return
        valid_speeds = [s for s in self.download_speeds if s > 0]
        if len(valid_speeds) < 2:
            return
        self.speed_min = min(valid_speeds)
        self.speed_max = max(valid_speeds)
        self.download_speed = statistics.mean(valid_speeds)
        self.speed_std = statistics.stdev(valid_speeds)
        if self.download_speed > 0:
            self.speed_cv = (self.speed_std / self.download_speed) * 100
        if self.speed_cv < 5:
            self.stability_score = 100 - self.speed_cv
        elif self.speed_cv < 10:
            self.stability_score = 90 - (self.speed_cv - 5) * 4
        elif self.speed_cv < 20:
            self.stability_score = 70 - (self.speed_cv - 10) * 2
        else:
            self.stability_score = max(0, 50 - (self.speed_cv - 20))
    
    def calculate_composite_score(self, speed_weight=0.4, stability_weight=0.3, latency_weight=0.3, max_speed=150):
        if self.download_speed > 0:
            normalized_speed = min(self.download_speed / max_speed * 100, 100)
            self.speed_score = normalized_speed * (0.7 + 0.3 * math.log10(1 + self.download_speed / 10))
            self.speed_score = min(100, self.speed_score)
        else:
            self.speed_score = 0
        if self.latency <= 10:
            self.latency_score = 100
        elif self.latency <= 20:
            self.latency_score = 100 - (self.latency - 10) * 2
        elif self.latency <= 50:
            self.latency_score = 80 - (self.latency - 20) * 1.5
        elif self.latency <= 100:
            self.latency_score = 35 - (self.latency - 50) * 0.5
        else:
            self.latency_score = max(0, 10 - (self.latency - 100) * 0.1)
        self.composite_score = (self.speed_score * speed_weight + self.stability_score * stability_weight + self.latency_score * latency_weight)
    
    def get_stability_stars(self):
        if self.speed_cv < 3: return "⭐⭐⭐⭐⭐"
        elif self.speed_cv < 5: return "⭐⭐⭐⭐"
        elif self.speed_cv < 10: return "⭐⭐⭐"
        elif self.speed_cv < 20: return "⭐⭐"
        else: return "⭐"
    
    def get_latency_grade(self):
        if self.latency <= 10: return "优秀"
        elif self.latency <= 20: return "良好"
        elif self.latency <= 50: return "一般"
        elif self.latency <= 100: return "较慢"
        else: return "很慢"
    
    def get_recommendation(self):
        if self.composite_score >= 85: return "⭐⭐⭐⭐⭐ 强烈推荐"
        elif self.composite_score >= 70: return "⭐⭐⭐⭐ 推荐使用"
        elif self.composite_score >= 55: return "⭐⭐⭐ 可以使用"
        elif self.composite_score >= 40: return "⭐⭐ 不太推荐"
        else: return "⭐ 不推荐"

class PingExecutor(ABC):
    @abstractmethod
    def execute(self, ip_list: List[str]) -> List[TestResult]:
        pass

class SyncPingExecutor(PingExecutor):
    def __init__(self, tester):
        self.tester = tester
    
    def execute(self, ip_list: List[str]) -> List[TestResult]:
        results, lock = [], threading.Lock()
        def worker(ip):
            result = self.tester.tcp_ping(ip)
            if result:
                latency, loss_rate = result
                if (self.tester.args.tll <= latency <= self.tester.args.tl and loss_rate <= self.tester.args.tlr):
                    with lock:
                        results.append(TestResult(ip, latency, loss_rate))
        with ThreadPoolExecutor(max_workers=self.tester.args.n) as executor:
            list(tqdm(executor.map(worker, ip_list), total=len(ip_list), desc="  ⏱️  Ping测试", ncols=80, leave=False))
        return results

class AsyncPingExecutor(PingExecutor):
    def __init__(self, tester):
        self.tester = tester
    
    async def tcp_ping_async(self, ip: str, semaphore: asyncio.Semaphore):
        success_count, latencies = 0, []
        for i in range(self.tester.args.t):
            try:
                async with semaphore:
                    start = time.perf_counter()
                    reader, writer = await asyncio.wait_for(asyncio.open_connection(ip, self.tester.args.tp), timeout=self.tester.args.timeout)
                    latency_ms = (time.perf_counter() - start) * 1000
                    try:
                        writer.close()
                        await writer.wait_closed()
                    except: pass
                success_count += 1
                latencies.append(latency_ms)
            except:
                if i == 0: return None
                continue
            finally:
                if i < self.tester.args.t - 1:
                    await asyncio.sleep(0.001)
        if success_count == 0: return None
        return sum(latencies) / len(latencies), 1 - (success_count / self.tester.args.t)
    
    async def worker_async(self, ip: str, semaphore: asyncio.Semaphore):
        result = await self.tcp_ping_async(ip, semaphore)
        if result:
            latency, loss_rate = result
            if (self.tester.args.tll <= latency <= self.tester.args.tl and loss_rate <= self.tester.args.tlr):
                return TestResult(ip, latency, loss_rate)
        return None
    
    async def execute_async(self, ip_list: List[str]):
        semaphore = asyncio.Semaphore(self.tester.args.async_ping_limit)
        tasks = [self.worker_async(ip, semaphore) for ip in ip_list]
        results = []
        with tqdm(total=len(ip_list), desc="  ⏱️  Ping测试(异步)", ncols=80, leave=False) as pbar:
            for coro in asyncio.as_completed(tasks):
                result = await coro
                if result: results.append(result)
                pbar.update(1)
        return results
    
    def execute(self, ip_list: List[str]):
        return asyncio.run(self.execute_async(ip_list))

class DownloadExecutor:
    def __init__(self, tester):
        self.tester = tester
    
    def download_test_with_progress(self, ip: str, test_num: int, total_tests: int):
        hostname, port = self.tester.parsed_url.hostname, self.tester.args.tp
        path = self.tester.parsed_url.path
        if self.tester.parsed_url.query:
            path += f"?{self.tester.parsed_url.query}"
        try:
            sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            sock.settimeout(5)
            sock.connect((ip, port))
            if self.tester.parsed_url.scheme == 'https':
                context = ssl.create_default_context()
                context.check_hostname = False
                context.verify_mode = ssl.CERT_NONE
                sock = context.wrap_socket(sock, server_hostname=hostname)
            request = f"GET {path} HTTP/1.1\r\nHost: {hostname}\r\nUser-Agent: Mozilla/5.0\r\nConnection: close\r\n\r\n"
            sock.sendall(request.encode())
            downloaded, start_time, last_update = 0, time.perf_counter(), time.perf_counter()
            buffer = b""
            while b"\r\n\r\n" not in buffer:
                chunk = sock.recv(4096)
                if not chunk: break
                buffer += chunk
            if b"\r\n\r\n" in buffer:
                downloaded = len(buffer) - buffer.find(b"\r\n\r\n") - 4
            while True:
                elapsed = time.perf_counter() - start_time
                if elapsed >= self.tester.args.dt: break
                try:
                    chunk = sock.recv(32768)
                    if not chunk: break
                    downloaded += len(chunk)
                    current_time = time.perf_counter()
                    if current_time - last_update >= 0.5:
                        current_speed = downloaded / (current_time - start_time) / 1024 / 1024
                        progress = int((elapsed / self.tester.args.dt) * 100)
                        bar = "█" * (progress // 5) + "░" * (20 - progress // 5)
                        print(f"\r    第 {test_num}/{total_tests} 次: [{bar}] {progress}% | 当前: {current_speed:.2f} MB/s", end="", flush=True)
                        last_update = current_time
                except: break
            sock.close()
            elapsed = time.perf_counter() - start_time
            final_speed = (downloaded / elapsed / 1024 / 1024) if elapsed > 0 else 0.0
            print(f"\r    第 {test_num}/{total_tests} 次: [{'█'*20}] 100% | 完成: {final_speed:.2f} MB/s" + " "*20)
            return final_speed
        except Exception as e:
            print(f"\r    第 {test_num}/{total_tests} 次: 下载失败" + " "*50)
            return 0.0
    
    def execute(self, test_ips: List[str]):
        print(f"📥 下载模式: 同步串行（最准确）✅\n⚙️  每IP测试: {self.tester.args.dc}次 × {self.tester.args.dt}秒\n")
        for i, ip in enumerate(test_ips, 1):
            print(f"\n[{i}/{len(test_ips)}] ({i*100//len(test_ips)}%) 测试 {ip}...")
            speeds = []
            for test_num in range(self.tester.args.dc):
                speed = self.download_test_with_progress(ip, test_num + 1, self.tester.args.dc)
                speeds.append(speed)
                if speed == 0 and test_num == 0: break
                if test_num < self.tester.args.dc - 1 and speed > 0: time.sleep(0.5)
            self.tester.update_result(ip, download_speeds=speeds)
            result = self.tester.results_dict.get(ip)
            if result:
                result.calculate_stability_metrics()
                max_speed = max([r.download_speed for r in self.tester.results if r.download_speed > 0], default=100)
                result.calculate_composite_score(self.tester.args.speed_weight, self.tester.args.stability_weight, self.tester.args.latency_weight, max_speed)
                valid_speeds = [s for s in speeds if s > 0]
                print(f"\n    {'━'*50}")
                if len(valid_speeds) > 1:
                    print(f"    📊 平均: {result.download_speed:.2f} MB/s | 范围: {result.speed_min:.2f}-{result.speed_max:.2f}")
                    print(f"    🎯 稳定: {result.get_stability_stars()} (CV: {result.speed_cv:.1f}%) | 延迟: {result.latency:.1f}ms")
                    print(f"    🏆 综合得分: {result.composite_score:.1f}/100")
                elif len(valid_speeds) == 1:
                    print(f"    ✅ 速度: {valid_speeds[0]:.2f} MB/s | 延迟: {result.latency:.1f}ms")
                else:
                    print(f"    ❌ 下载失败")
                print(f"    {'━'*50}")
            if i < len(test_ips):
                remaining = (len(test_ips) - i) * self.tester.args.dc * self.tester.args.dt
                print(f"    ⏱️  剩余: 约{remaining}秒\n")
            if i % 3 == 0 or i == len(test_ips):
                self.tester.update_best_ips()

class CloudflareSpeedTest:
    def __init__(self):
        self.results, self.results_dict, self.lock = [], {}, threading.Lock()
        self.args = self.parse_args()
        self.debug, self.total_ips_tested = self.args.debug, 0
        self.parsed_url = urlparse(self.args.url)
        self.validate_args()
        self.ping_executor = AsyncPingExecutor(self) if self.args.async_ping else SyncPingExecutor(self)
        self.download_executor = DownloadExecutor(self)
    
    def parse_args(self):
        parser = argparse.ArgumentParser(description=f"CloudflareSpeedTest {VERSION}")
        parser.add_argument("-n", type=int, default=200, help="并发线程数")
        parser.add_argument("-t", type=int, default=3, help="Ping次数")
        parser.add_argument("-dn", type=int, default=10, help="下载测试IP数")
        parser.add_argument("-dt", type=int, default=10, help="下载时长/秒")
        parser.add_argument("-dc", type=int, default=5, help="下载次数")
        parser.add_argument("-tp", type=int, default=443, help="端口")
        parser.add_argument("-url", default=DEFAULT_URL, help="测试URL")
        parser.add_argument("--timeout", type=int, default=5, help="超时")
        parser.add_argument("--async-ping", action="store_true", help="异步Ping")
        parser.add_argument("--async-ping-limit", type=int, default=512, help="异步并发数")
        parser.add_argument("-tl", type=int, default=9999, help="最大延迟")
        parser.add_argument("-tll", type=int, default=0, help="最小延迟")
        parser.add_argument("-tlr", type=float, default=0.0, help="最大丢包率")
        parser.add_argument("--max-cv", type=float, default=100, help="最大CV百分比")
        parser.add_argument("--min-stability", type=float, default=0, help="最小稳定性")
        parser.add_argument("--speed-weight", type=float, default=0.5, help="速度权重(默认0.5)")
        parser.add_argument("--stability-weight", type=float, default=0.3, help="稳定性权重(默认0.3)")
        parser.add_argument("--latency-weight", type=float, default=0.2, help="延迟权重(默认0.2)")
        parser.add_argument("--sort", choices=['composite','speed','stability','latency'], default='composite', help="排序")
        parser.add_argument("-f", default="ip.txt", help="IP文件")
        parser.add_argument("-o", default="", help="输出文件")
        parser.add_argument("-ip", default="", help="指定IP")
        parser.add_argument("-dd", action="store_true", help="禁用下载")
        parser.add_argument("-v", action="store_true", help="版本")
        parser.add_argument("--debug", action="store_true", help="调试")
        parser.add_argument("--max-ips", type=int, default=50000, help="最大IP数")
        parser.add_argument("--sample-rate", type=float, default=1.0, help="采样倍率")
        args = parser.parse_args()
        total_weight = args.speed_weight + args.stability_weight + args.latency_weight
        if abs(total_weight - 1.0) > 0.01:
            args.speed_weight /= total_weight
            args.stability_weight /= total_weight
            args.latency_weight /= total_weight
        return args
    
    def validate_args(self):
        if self.args.async_ping and not ASYNC_AVAILABLE:
            print("❌ 需要Python 3.7+")
            sys.exit(1)
    
    def tcp_ping(self, ip: str):
        success_count, latencies = 0, []
        for i in range(self.args.t):
            try:
                start = time.perf_counter()
                with socket.create_connection((ip, self.args.tp), timeout=self.args.timeout):
                    latencies.append((time.perf_counter() - start) * 1000)
                    success_count += 1
            except:
                if i == 0: return None
        if success_count == 0: return None
        return sum(latencies) / len(latencies), 1 - (success_count / self.args.t)
    
    def update_result(self, ip: str, **kwargs):
        with self.lock:
            if ip in self.results_dict:
                for k, v in kwargs.items():
                    setattr(self.results_dict[ip], k, v)
    
    def update_best_ips(self):
        key = {'composite': lambda x: x.composite_score, 'speed': lambda x: x.download_speed, 'stability': lambda x: x.stability_score, 'latency': lambda x: x.latency}[self.args.sort]
        reverse = self.args.sort != 'latency'
        sorted_results = sorted([r for r in self.results if (r.composite_score if self.args.sort=='composite' else r.download_speed) > 0], key=key, reverse=reverse)[:3]
        if sorted_results:
            print(f"\n{'━'*80}\n🏆 当前最佳IP\n{'━'*80}")
            for i, r in enumerate(sorted_results, 1):
                print(f"{i}. {r.ip:<16} {r.download_speed:>6.2f}MB/s  {r.latency:>5.1f}ms  {r.get_stability_stars()}  综合:{r.composite_score:>5.1f}")
            print(f"{'━'*80}\n")
    
    def load_ip_ranges(self):
        if self.args.ip: return self.args.ip.split(",")
        if not os.path.exists(self.args.f):
            print(f"❌ 文件不存在: {self.args.f}")
            sys.exit(1)
        with open(self.args.f, "r", encoding='utf-8') as f:
            return [line.strip() for line in f if line.strip() and not line.startswith("#")]
    
    def generate_test_ips(self, ip_ranges: List[str]):
        count = 0
        for ip_range in ip_ranges:
            if count >= self.args.max_ips: break
            if "/" in ip_range:
                try:
                    network = ipaddress.ip_network(ip_range, strict=False)
                    hosts = list(network.hosts())
                    
                    # 根据网段大小和采样倍率计算采样数
                    if network.prefixlen <= 16:
                        # /16 大网段：采样更多
                        base_sample = min(len(hosts), 500)
                    elif network.prefixlen <= 20:
                        # /17-/20 中等网段
                        base_sample = min(len(hosts), 200)
                    elif network.prefixlen <= 24:
                        # /21-/24 小网段
                        base_sample = min(len(hosts), 50)
                    else:
                        # /25+ 很小网段：全部测试
                        base_sample = len(hosts)
                    
                    # 应用采样倍率
                    sample_count = int(min(base_sample * self.args.sample_rate, len(hosts)))
                    
                    if self.debug:
                        print(f"  {ip_range} (/{network.prefixlen}): 总数{len(hosts)}, 采样{sample_count}")
                    
                    for ip in random.sample(hosts, sample_count):
                        yield str(ip)
                        count += 1
                        if count >= self.args.max_ips: break
                except:
                    pass
            else:
                try:
                    ipaddress.ip_address(ip_range)
                    yield ip_range
                    count += 1
                except:
                    pass
    
    def print_results(self):
        print(f"\n{'='*80}\n📊 详细结果 - 三维评分\n{'='*80}\n")
        if not self.results:
            print("❌ 无结果")
            return
        filtered = [r for r in self.results if r.speed_cv <= self.args.max_cv and r.stability_score >= self.args.min_stability and r.download_speed > 0]
        if not filtered:
            print("❌ 无符合条件的IP")
            return
        key = {'composite': lambda x: x.composite_score, 'speed': lambda x: x.download_speed, 'stability': lambda x: x.stability_score, 'latency': lambda x: x.latency}[self.args.sort]
        filtered.sort(key=key, reverse=(self.args.sort != 'latency'))
        
        print(f"⚖️  评分权重: 速度{self.args.speed_weight:.0%} + 稳定{self.args.stability_weight:.0%} + 延迟{self.args.latency_weight:.0%}\n")
        print(f"{'IP':<16} {'速度':>8} {'延迟':>7} {'范围':>18} {'CV%':>5} {'稳定':>10} {'综合':>6}")
        print(f"{'-'*16} {'-'*8} {'-'*7} {'-'*18} {'-'*5} {'-'*10} {'-'*6}")
        
        for r in filtered[:self.args.dn]:
            speed_range = f"{r.speed_min:.1f}-{r.speed_max:.1f}"
            print(f"{r.ip:<16} {r.download_speed:>7.2f}M {r.latency:>6.1f}ms {speed_range:>18} {r.speed_cv:>5.1f} {r.get_stability_stars():>10} {r.composite_score:>6.1f}")
        
        print(f"\n{'='*80}\n")
        
        if filtered:
            best = filtered[0]
            print(f"🎯 最佳推荐: {best.ip}")
            print(f"   综合得分: {best.composite_score:.1f}/100")
            print(f"   • 速度: {best.download_speed:.2f} MB/s (得分 {best.speed_score:.1f})")
            print(f"   • 稳定性: {best.get_stability_stars()} CV {best.speed_cv:.1f}% (得分 {best.stability_score:.1f})")
            print(f"   • 延迟: {best.latency:.2f}ms {best.get_latency_grade()} (得分 {best.latency_score:.1f})")
    
    def save_results(self):
        if not self.args.o:
            return
        
        filtered = [r for r in self.results if r.speed_cv <= self.args.max_cv and r.stability_score >= self.args.min_stability]
        key = {'composite': lambda x: x.composite_score, 'speed': lambda x: x.download_speed, 'stability': lambda x: x.stability_score, 'latency': lambda x: x.latency}[self.args.sort]
        filtered.sort(key=key, reverse=(self.args.sort != 'latency'))
        
        if self.args.o.endswith('.csv'):
            with open(self.args.o, 'w', newline='', encoding='utf-8-sig') as f:
                writer = csv.writer(f)
                writer.writerow(['IP','延迟ms','丢包%','平均MB/s','最小','最大','标准差','CV%','速度分','稳定分','延迟分','综合分'])
                for r in filtered[:self.args.dn]:
                    writer.writerow([r.ip, f"{r.latency:.2f}", f"{r.loss_rate*100:.1f}", f"{r.download_speed:.2f}", f"{r.speed_min:.2f}", f"{r.speed_max:.2f}", f"{r.speed_std:.2f}", f"{r.speed_cv:.2f}", f"{r.speed_score:.1f}", f"{r.stability_score:.1f}", f"{r.latency_score:.1f}", f"{r.composite_score:.1f}"])
            print(f"💾 已保存: {self.args.o}")
        
        elif self.args.o.endswith('.json'):
            data = {
                'test_info': {
                    'version': VERSION,
                    'test_time': datetime.now().strftime("%Y-%m-%d %H:%M:%S"),
                    'total_ips': self.total_ips_tested,
                    'weights': {'speed': self.args.speed_weight, 'stability': self.args.stability_weight, 'latency': self.args.latency_weight}
                },
                'results': []
            }
            for r in filtered[:self.args.dn]:
                data['results'].append({
                    'ip': r.ip,
                    'latency_ms': round(r.latency, 2),
                    'download_mbps': round(r.download_speed, 2),
                    'stability_cv': round(r.speed_cv, 2),
                    'composite_score': round(r.composite_score, 1)
                })
            with open(self.args.o, 'w', encoding='utf-8') as f:
                json.dump(data, f, indent=2, ensure_ascii=False)
            print(f"💾 已保存: {self.args.o}")
        
        elif self.args.o.endswith('.txt'):
            with open(self.args.o, 'w', encoding='utf-8') as f:
                f.write(f"CloudflareSpeedTest {VERSION} - 测试结果\n")
                f.write(f"{'='*80}\n")
                f.write(f"测试时间: {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}\n")
                f.write(f"评分权重: 速度{self.args.speed_weight:.0%} + 稳定{self.args.stability_weight:.0%} + 延迟{self.args.latency_weight:.0%}\n")
                f.write(f"{'='*80}\n\n")
                
                for i, r in enumerate(filtered[:self.args.dn], 1):
                    f.write(f"[{i}] {r.ip}\n")
                    f.write(f"  延迟: {r.latency:.2f}ms ({r.get_latency_grade()})\n")
                    f.write(f"  速度: {r.download_speed:.2f} MB/s (范围: {r.speed_min:.2f}-{r.speed_max:.2f})\n")
                    f.write(f"  稳定性: {r.get_stability_stars()} (CV: {r.speed_cv:.1f}%)\n")
                    f.write(f"  综合得分: {r.composite_score:.1f}/100\n")
                    f.write(f"  {r.get_recommendation()}\n")
                    f.write(f"{'-'*80}\n")
                
                if filtered:
                    f.write(f"\n🎯 最佳推荐: {filtered[0].ip}\n")
                    f.write(f"   综合得分: {filtered[0].composite_score:.1f}/100\n")
            print(f"💾 已保存: {self.args.o}")
        
        else:
            # 默认保存为CSV
            self.args.o += '.csv'
            self.save_results()
    
    def run(self):
        print(f"{'='*80}\n🚀 CloudflareSpeedTest {VERSION}\n{'='*80}")
        ip_ranges = self.load_ip_ranges()
        print(f"📂 IP范围: {len(ip_ranges)} | 最大测试: {self.args.max_ips}")
        print(f"⚖️  权重: 速度{self.args.speed_weight:.0%} + 稳定{self.args.stability_weight:.0%} + 延迟{self.args.latency_weight:.0%}\n")
        
        # 阶段1: Ping测试
        print(f"{'='*80}\n🔍 阶段1: Ping测试\n{'='*80}\n")
        ip_list = list(self.generate_test_ips(ip_ranges))
        self.total_ips_tested = len(ip_list)
        
        if not ip_list:
            print("❌ 没有可测试的IP！")
            return
        
        print(f"📊 准备测试 {len(ip_list)} 个IP...\n")
        results = self.ping_executor.execute(ip_list)
        
        with self.lock:
            self.results.extend(results)
            for r in results:
                self.results_dict[r.ip] = r
        
        self.results.sort(key=lambda x: x.latency)
        print(f"\n✅ 完成: {len(self.results)}个IP通过筛选\n")
        
        # 如果没有通过Ping的IP，退出
        if not self.results:
            print("❌ 没有IP通过延迟测试！")
            return
        
        # 阶段2: 下载测试
        if not self.args.dd:
            print(f"{'='*80}\n🚀 阶段2: 下载测试\n{'='*80}\n")
            test_ips = [r.ip for r in self.results[:self.args.dn]]
            self.download_executor.execute(test_ips)
            print("\n✅ 完成!\n")
    
    def main(self):
        if self.args.v:
            print(f"CloudflareSpeedTest {VERSION}")
            return
        try:
            start = time.time()
            self.run()
            self.print_results()
            self.save_results()
            print(f"\n⏱️  耗时: {time.time()-start:.1f}秒\n{'='*80}\n")
        except KeyboardInterrupt:
            print("\n⚠️  已中断")
            if self.results and self.args.o: self.save_results()
            sys.exit(0)

if __name__ == "__main__":
    CloudflareSpeedTest().main()
