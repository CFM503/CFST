import socket
import ssl
import time

ip = "104.26.14.73"
port = 443

def test_download():
    try:
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.settimeout(10.0)
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        s = ctx.wrap_socket(sock, server_hostname='speed.cloudflare.com')
        
        print(f"Connecting to {ip}...")
        s.connect((ip, port))
        print("Connected.")
        
        req = b"GET /__down?bytes=1000000 HTTP/1.1\r\nHost: speed.cloudflare.com\r\nUser-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0\r\nConnection: close\r\n\r\n"
        s.sendall(req)
        print("Request sent.")
        
        data = s.recv(4096)
        print(f"Received {len(data)} bytes.")
        print("Response header:")
        print(data.decode('utf-8', errors='ignore'))
        
        s.close()
    except Exception as e:
        print(f"Error: {e}")

if __name__ == "__main__":
    test_download()
