#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
CloudflareSpeedTest (cfst) - v5.1 Colo Edition
新特性：数据中心识别 (Colo) + C段去重 + 大带宽支持
"""

import os
import sys
import time
import socket
import ssl
import random
import argparse
import threading
import ipaddress
import csv
import re
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from typing import List, Optional

# -----------------------------------------------------------------------------
# Configuration
# -----------------------------------------------------------------------------
VERSION = "v5.1-Colo"
USER_AGENT = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0 SpeedTest"

CLOUDFLARE_IPV4_RANGES = [
    "173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
    "141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
    "197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
    "104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22"
]

class Color:
    RESET = "\033[0m"
    RED = "\033[91m"
    GREEN = "\033[92m"
    YELLOW = "\033[93m"
    CYAN = "\033[96m"
    MAGENTA = "\033[95m"
    
    @staticmethod
    def colorize(text, color_code):
        if sys.platform == 'win32' and os.getenv('TERM') != 'xterm': return text
        return f"{color_code}{text}{Color.RESET}"

@dataclass
class NodeResult:
    ip: str
    port: int
    tcp_latency: float = 0.0
    download_speed: float = 0.0
    colo: str = "UNK"  # 数据中心代码 (如 LAX)
    score: float = 0.0

    def calculate_score(self):
        # 评分算法 v5.1
        score_speed = min(100, (self.download_speed / 40.0) * 100)
        score_latency = max(0, 100 - (self.tcp_latency - 30) * 0.5)
        # 如果获取到了 Colo，稍微加一点分以示奖励（数据更完整）
        bonus = 5 if self.colo != "UNK" else 0
        self.score = score_speed * 0.8 + score_latency * 0.2 + bonus

# -----------------------------------------------------------------------------
# Network Engines
# -----------------------------------------------------------------------------
class NetworkEngine:
    def __init__(self, ip, port, timeout=2.0):
        self.ip = ip
        self.port = port
        self.timeout = timeout

    def tcp_ping(self) -> float:
        try:
            start = time.perf_counter()
            s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            s.settimeout(self.timeout)
            s.connect((self.ip, self.port))
            s.close()
            return (time.perf_counter() - start) * 1000
        except: return 0.0

    def get_colo(self) -> str:
        """获取数据中心代码 (Colo)"""
        try:
            s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            s.settimeout(2.0)
            if self.port == 443:
                ctx = ssl.create_default_context()
                ctx.check_hostname = False
                ctx.verify_mode = ssl.CERT_NONE
                s = ctx.wrap_socket(s, server_hostname='speed.cloudflare.com')
            
            s.connect((self.ip, self.port))
            req = b"GET /cdn-cgi/trace HTTP/1.1\r\nHost: speed.cloudflare.com\r\nUser-Agent: CFST/5.1\r\nConnection: close\r\n\r\n"
            s.sendall(req)
            
            data = b""
            while True:
                chunk = s.recv(4096)
                if not chunk: break
                data += chunk
                if b"colo=" in data: break
            s.close()
            
            # 解析 colo=XXX
            text = data.decode('utf-8', errors='ignore')
            m = re.search(r'colo=([A-Z]+)', text)
            if m: return m.group(1)
            return "UNK"
        except:
            return "ERR"

    def multi_thread_download(self, threads=4, duration=8) -> float:
        def _download_worker():
            try:
                sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
                sock.settimeout(5.0)
                if self.port == 443:
                    ctx = ssl.create_default_context()
                    ctx.check_hostname = False
                    ctx.verify_mode = ssl.CERT_NONE
                    s = ctx.wrap_socket(sock, server_hostname='speed.cloudflare.com')
                else: s = sock
                
                s.connect((self.ip, self.port))
                # 请求 2GB 文件防止跑完
                req = b"GET /__down?bytes=2000000000 HTTP/1.1\r\nHost: speed.cloudflare.com\r\nConnection: keep-alive\r\n\r\n"
                s.sendall(req)
                
                start_t = time.perf_counter()
                downloaded = 0
                while True:
                    if time.perf_counter() - start_t > duration: break
                    chunk = s.recv(65536)
                    if not chunk: break
                    downloaded += len(chunk)
                s.close()
                return downloaded
            except Exception as e: 
                print(f"Error: {e}")
                return 0

        total_bytes = 0
        start_global = time.perf_counter()
        with ThreadPoolExecutor(max_workers=threads) as executor:
            futures = [executor.submit(_download_worker) for _ in range(threads)]
            for f in as_completed(futures):
                total_bytes += f.result()
        
        real_time = time.perf_counter() - start_global
        return (total_bytes / 1024 / 1024) / max(0.1, real_time)

# -----------------------------------------------------------------------------
# Main Logic
# -----------------------------------------------------------------------------
class CFSTApp:
    def __init__(self):
        self.args = self._parse_args()
    
    def _parse_args(self):
        # 详细的双语帮助信息 / Detailed Bilingual Help
        epilog_text = """
=============================================================================
使用示例 / Usage Examples:

1. 🚀 默认极速模式 (Default Fast Mode):
   python cfst.py
   (扫描2000个IP -> 测速延迟最低的10个 -> 4线程下载 -> 6秒)

2. ⚡ 暴力测速模式 (High Performance Mode):
   python cfst.py -c 8 -dt 10 -dn 20
   (8线程并发, 测速10秒,以此找出前20个最快IP / 8 threads, 10s duration, top 20)

3. 🎯 指定数量与阈值 (Custom Quantity & Threshold):
   python cfst.py -max 5000 -st 50.0
   (扫描5000个IP, 速度超过50MB/s即停止 / Scan 5000 IPs, stop if speed > 50MB/s)

4. 📂 使用自定义IP文件 (Custom IP File):
   python cfst.py -f ip.txt
   (从ip.txt读取IP段 / Read IPs from ip.txt)

6. 🧊 C段去重模式 (Unique Subnet):
   python cfst.py -u -max 5000
   (保证每个IP来自不同的C段子网 / Diverse IP subnets)

-----------------------------------------------------------------------------
参数说明 / Arguments:
  -f,   --file            IP文件路径 / IP range file path
  -p,   --port            目标端口 / Target port (Default: 443)
  -max, --max-scan        扫描IP总数 / Max IPs to scan (Default: 2000)
  -c,   --conc            下载并发线程 / Download threads (Default: 4)
  -dn,  --download-num    下载测速数量 / IPs to download test (Default: 10)
  -dt,  --duration        测速时长(秒) / Test duration (sec) (Default: 6)
  -st,  --stop-threshold  极速熔断阈值 / Speed threshold to stop (MB/s) (Default: 25.0)
  -u,   --unique          C段去重 / Ensure each IP is from a different subnet
  -o,   --output          结果保存文件 / Output file (Default: result_colo.csv)
=============================================================================
        """
        parser = argparse.ArgumentParser(
            description="CloudflareSpeedTest v5.1-Colo (Ultimate Edition)",
            epilog=epilog_text,
            formatter_class=argparse.RawDescriptionHelpFormatter,
            add_help=False
        )
        p = parser.add_argument
        p('-f', '--file', help="IP文件 / IP file")
        p('-p', '--port', type=int, default=443, help="端口 / Port")
        p('-max', '--max-scan', type=int, default=2000, help="扫描数 / Max scan")
        p('-c', '--conc', type=int, default=4, help="并发数 / Threads")
        p('-dn', '--download-num', type=int, default=10, help="测速数 / Download count")
        p('-dt', '--duration', type=int, default=6, help="时长 / Duration")
        p('-st', '--stop-threshold', type=float, default=25.0, help="熔断阈值 / Stop threshold")
        p('-u', '--unique', action='store_true', help="C段去重 / Unique Subnet")
        p('-o', '--output', default='result_colo.csv', help="输出文件 / Output file")
        p('-H', '--help', action='help', help="显示帮助 / Show help")
        return parser.parse_args()

    def generate_random_ips(self):
        """生成随机IP (支持去重模式)"""
        ips = []
        ranges = CLOUDFLARE_IPV4_RANGES
        if self.args.file:
             with open(self.args.file) as f: ranges = [l.strip() for l in f if l.strip()]
        
        targets = self.args.max_scan
        
        # 模式1: C段去重 (Unique Subnet Mode)
        if self.args.unique:
            seen_subnets = set()
            attempts = 0
            while len(ips) < targets and attempts < targets * 5 and ranges:
                attempts += 1
                cidr = random.choice(ranges)
                try:
                    if '/' in cidr:
                        net = ipaddress.ip_network(cidr, strict=False)
                        rand_ip = net[random.randint(1, net.num_addresses - 2)]
                        ip_str = str(rand_ip)
                        subnet = ".".join(ip_str.split(".")[:3])
                        if subnet not in seen_subnets:
                            seen_subnets.add(subnet)
                            ips.append(ip_str)
                    else:
                        if cidr not in ips: ips.append(cidr)
                except: pass
            return ips

        # 模式2: 常规随机 (Default Random Mode)
        per_range = int(targets / len(ranges)) + 3
        for r in ranges:
            try:
                if '/' in r:
                    net = ipaddress.ip_network(r, strict=False)
                    for _ in range(per_range):
                        rand = random.randint(1, net.num_addresses - 2)
                        ips.append(str(net[rand]))
                else: ips.append(r)
            except: pass
        
        random.shuffle(ips)
        return ips[:targets]

    def run(self):
        print(f"Cloudflare SpeedTest {VERSION} (Colo)")
        if sys.platform == 'win32':
             try:
                 sys.stdout.reconfigure(encoding='utf-8')
                 os.system('color')
             except: pass

        # 1. 扫描
        ips = self.generate_random_ips()
        dedup_msg = " [C段去重]" if self.args.unique else ""
        print(f"🔍 扫描 {len(ips)} 个 IP{dedup_msg} (并发延迟测试)...")
        
        valid_nodes = []
        lock = threading.Lock()
        done = 0
        
        def _ping(ip):
            nonlocal done
            eng = NetworkEngine(ip, self.args.port, timeout=1.0)
            lat = eng.tcp_ping()
            with lock:
                done += 1
                sys.stdout.write(f"\rProcess: {done}/{len(ips)} | Valid: {len(valid_nodes)}")
                sys.stdout.flush()
                if lat > 0: valid_nodes.append(NodeResult(ip, self.args.port, tcp_latency=lat))

        with ThreadPoolExecutor(max_workers=200) as exe:
            list(exe.map(_ping, ips))
        print("\n")

        # 2. 获取 Colo (针对 Top N*2 候选者)
        # 多拿一倍候选者，以防获取Colo失败
        candidates = sorted(valid_nodes, key=lambda x: x.tcp_latency)[:self.args.download_num * 2]
        
        print(f"🌐 正在识别数据中心 (Top {len(candidates)})...")
        def _fill_colo(node):
            eng = NetworkEngine(node.ip, node.port)
            node.colo = eng.get_colo()
        
        with ThreadPoolExecutor(max_workers=20) as exe:
            list(exe.map(_fill_colo, candidates))
            
        # 过滤掉 ERR 的节点 (可选，目前保留)
        final_candidates = candidates[:self.args.download_num]

        # 3. 测速
        print(f"\n🚀 多线程并发测速 ({self.args.conc}线程, {self.args.duration}秒)")
        print(f"{'IP':<16} {'Colo':<6} {'Latency':<8} {'Speed':<20} {'Score':<6}")
        print("-" * 65)
        
        results = []
        fast_count = 0
        
        for node in final_candidates:
            eng = NetworkEngine(node.ip, node.port)
            speed = eng.multi_thread_download(threads=self.args.conc, duration=self.args.duration)
            node.download_speed = speed
            node.calculate_score()
            results.append(node)
            
            s_str = f"{speed:.2f} MB/s"
            if speed > 20: s_str = f"{Color.GREEN}{s_str}{Color.RESET}"
            elif speed > 5: s_str = f"{Color.YELLOW}{s_str}{Color.RESET}"
            
            colo_str = f"{Color.MAGENTA}{node.colo}{Color.RESET}"
            print(f"{node.ip:<16} {colo_str:<15} {node.tcp_latency:5.1f}ms  {s_str:<29} {node.score:.1f}")
            
            if speed >= self.args.stop_threshold:
                fast_count += 1
                if fast_count >= 5: 
                    print("\n⚡ 满足熔断条件，停止")
                    break

        # 保存
        results.sort(key=lambda x: x.score, reverse=True)
        with open(self.args.output, 'w', newline='', encoding='utf-8') as f:
            w = csv.writer(f)
            w.writerow(['IP','Colo','Latency','Speed_MB','Score'])
            for r in results:
                w.writerow([r.ip, r.colo, f"{r.tcp_latency:.1f}", f"{r.download_speed:.2f}", f"{r.score:.1f}"])
        print(f"\n💾 结果: {self.args.output}")

if __name__ == "__main__":
    # 强制 UTF-8 输出以修复 Windows 下 argparse 打印 Emoji 报错的问题
    # Force UTF-8 stdout to fix Emoji printing issues on Windows
    if sys.platform == 'win32':
        try:
            sys.stdout.reconfigure(encoding='utf-8')
            os.system('color') 
        except: pass

    try: 
        CFSTApp().run()
    except KeyboardInterrupt: pass
