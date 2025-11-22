#!/usr/bin/env python3
"""
CloudflareSpeedTest v3.10-AutoFix
修复说明：
- 智能识别端口与协议：
  - 当指定 -tp 80 时，自动将 HTTPS 降级为 HTTP，解决握手失败问题。
  - 当指定 -tp 443 时，强制使用 HTTPS。
- 错误显影：下载失败时直接打印错误原因，不再猜测。
"""
import os
import sys
import time
import random
import socket
import ipaddress
import argparse
import math
from urllib.parse import urlparse
from datetime import datetime
import warnings

try:
    from tqdm import tqdm
    import requests
    import urllib3
except ImportError:
    print("请安装依赖: pip install requests tqdm urllib3")
    sys.exit(1)

# 核心配置
ASYNC_AVAILABLE = sys.version_info >= (3, 7)
if ASYNC_AVAILABLE: import asyncio

warnings.filterwarnings('ignore')
urllib3.disable_warnings()

# 默认使用 200MB 文件测速
DEFAULT_URL = "https://speed.cloudflare.com/__down?bytes=200000000"
VERSION = "v3.10-AutoFix"

class CloudflareSpeedTest:
    def __init__(self):
        self.args = self.parse_args()
        self.parsed_url = urlparse(self.args.url)
        self.session = requests.Session()
        # 模拟真实浏览器头
        self.session.headers.update({
            'User-Agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0.0.0 Safari/537.36',
            'Accept': '*/*',
            'Connection': 'close'
        })
        
    def parse_args(self):
        p = argparse.ArgumentParser(description=f"CF Speed {VERSION}")
        p.add_argument("-n", type=int, default=200)
        p.add_argument("-t", type=int, default=3)
        p.add_argument("-dn", type=int, default=3) # 默认测3个
        p.add_argument("-dt", type=int, default=8)
        p.add_argument("-dc", type=int, default=1)
        p.add_argument("-nu", "--no-upload", action="store_true")
        p.add_argument("-tp", type=int, default=443)
        p.add_argument("-url", default=DEFAULT_URL)
        p.add_argument("--timeout", type=int, default=3)
        p.add_argument("--async-ping", action="store_true")
        p.add_argument("--async-ping-limit", type=int, default=512)
        p.add_argument("-f", default="ip.txt")
        p.add_argument("-o", "--output", default="")
        p.add_argument("--sample-rate", type=float, default=1.0)
        # 兼容参数
        p.add_argument("-dd", action="store_true")
        p.add_argument("-httping", action="store_true")
        p.add_argument("-httping-code", default="200")
        p.add_argument("--debug", action="store_true")
        p.add_argument("-tl", type=int, default=9999)
        p.add_argument("-tll", type=int, default=0)
        p.add_argument("-tlr", type=float, default=0.2)
        p.add_argument("--max-ips", type=int, default=50000)
        p.add_argument("--batch-size", type=int, default=5000)
        return p.parse_args()

    def _calc_stats(self, speeds):
        valid = [s for s in speeds if s > 0]
        if not valid: return 0.0, 0.0
        avg = sum(valid)/len(valid)
        std = math.sqrt(sum((x-avg)**2 for x in valid)/len(valid)) if len(valid)>1 else 0.0
        return avg, std

    # --- Async Ping ---
    async def _async_ping_worker(self, ip, sem):
        try:
            async with sem:
                s = time.perf_counter()
                _, w = await asyncio.wait_for(
                    asyncio.open_connection(ip, self.args.tp), 
                    timeout=self.args.timeout
                )
                lat = (time.perf_counter() - s) * 1000
                w.close()
                await w.wait_closed()
                return {'ip': ip, 'lat': lat}
        except: return None

    def run_async_ping(self, ips):
        if not ASYNC_AVAILABLE: return []
        loop = asyncio.new_event_loop()
        asyncio.set_event_loop(loop)
        sem = asyncio.Semaphore(self.args.async_ping_limit)
        tasks = [self._async_ping_worker(ip, sem) for ip in ips]
        
        results = []
        pbar = tqdm(total=len(ips), desc="  ⏱️  Ping (Async)", ncols=80, unit="ip")
        final_tasks = asyncio.gather(*tasks, return_exceptions=True)
        res_list = loop.run_until_complete(final_tasks)
        
        for r in res_list:
            pbar.update(1)
            if isinstance(r, dict): results.append(r)
        pbar.close()
        loop.close()
        return results

    # --- 核心测速 (修复版) ---
    def download_test_one(self, ip):
        # 智能协议判断
        original_scheme = self.parsed_url.scheme
        host = self.parsed_url.hostname
        path = self.parsed_url.path or "/"
        query = self.parsed_url.query
        
        # 关键修复：如果端口是80，强制使用 http，否则 requests 会尝试 SSL 握手导致失败
        if self.args.tp == 80:
            scheme = "http"
        elif self.args.tp == 443:
            scheme = "https"
        else:
            scheme = original_scheme
            
        target_url = f"{scheme}://{ip}:{self.args.tp}{path}"
        if query: target_url += f"?{query}"
        
        headers = {'Host': host}
        
        try:
            start_time = time.perf_counter()
            with self.session.get(target_url, headers=headers, stream=True, timeout=self.args.timeout, verify=False) as r:
                r.raise_for_status()
                total_bytes = 0
                for chunk in r.iter_content(chunk_size=16384):
                    total_bytes += len(chunk)
                    if time.perf_counter() - start_time > self.args.dt:
                        break
            
            duration = time.perf_counter() - start_time
            if duration <= 0: return 0.0
            speed = (total_bytes / 1024 / 1024) / duration
            return speed
            
        except Exception as e:
            # 错误显影：只显示简短的错误类型
            err_msg = str(e)
            if "SSLError" in err_msg: err_msg = "SSL握手失败(协议不匹配)"
            elif "timeout" in err_msg.lower(): err_msg = "连接超时"
            elif "reset" in err_msg.lower(): err_msg = "连接重置(被墙)"
            else: err_msg = err_msg[:20] + "..."
            print(f"\n    ⚠️  {ip} 失败: {err_msg}", end="")
            return 0.0

    def run_speed_test(self, results):
        print(f"\n🚀 开始测速 (Top {self.args.dn} IPs)")
        print(f"⚙️  模式: 纯下载 | 端口: {self.args.tp}")
        
        final_results = []
        for i, item in enumerate(results[:self.args.dn], 1):
            ip = item['ip']
            print(f"[{i}] 测试 {ip:<15} (Ping: {item['lat']:.0f}ms)...", end=" ", flush=True)
            
            speeds = []
            for j in range(self.args.dc):
                s = self.download_test_one(ip)
                speeds.append(s)
                if s == 0 and j == 0: break
                if j < self.args.dc - 1: time.sleep(0.2)
            
            avg, std = self._calc_stats(speeds)
            score = max(0, avg - std)
            
            if avg > 0:
                print(f"速度: {avg:.1f} MB/s (±{std:.1f})")
            else:
                print(" -> 0.0 MB/s") # 换行
            
            item['speed'] = avg
            item['std'] = std
            item['score'] = score
            final_results.append(item)
            
        final_results.sort(key=lambda x: x['score'], reverse=True)
        return final_results

    def main(self):
        print(f"CloudflareSpeedTest {VERSION}")
        
        ips = []
        if os.path.exists(self.args.f):
            with open(self.args.f) as f:
                raw_lines = [l.strip() for l in f if l.strip() and not l.startswith("#")]
            for line in raw_lines:
                if "/" in line:
                    try:
                        net = ipaddress.ip_network(line, strict=False)
                        hosts = list(net.hosts())
                        # 采样倍率
                        count = min(len(hosts), int(10 * self.args.sample_rate))
                        if count > 0: ips.extend([str(ip) for ip in random.sample(hosts, count)])
                    except: pass
                else:
                    ips.append(line)
            print(f"📂 生成 {len(ips)} IP (Rate: {self.args.sample_rate})")
        else: return

        if not ips: return

        if self.args.async_ping:
            ping_results = self.run_async_ping(ips)
        else:
            print("请加 --async-ping"); return

        ping_results.sort(key=lambda x: x['lat'])
        print(f"✅ 有效Ping: {len(ping_results)}")

        if not self.args.dd:
            final = self.run_speed_test(ping_results)
            print(f"\n{'='*60}")
            print(f"{'IP地址':<16} {'延迟':>6} | {'下载速度':>10} {'波动':>6} | {'得分':>5}")
            print(f"{'-'*16} {'-'*6} | {'-'*10} {'-'*6} | {'-'*5}")
            for r in final:
                print(f"{r['ip']:<16} {r['lat']:>6.0f} | {r['speed']:>10.1f} ±{r['std']:<5.1f} | {r['score']:>5.1f}")
            print(f"{'='*60}")

if __name__ == "__main__":
    CloudflareSpeedTest().main()