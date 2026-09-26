import socket
import time
from concurrent.futures import ThreadPoolExecutor

print("waiting 30s for board to boot and associate...", flush=True)
time.sleep(30)
print("scanning 192.168.31.0/24 port 4403...", flush=True)

def check(ip):
    try:
        s = socket.create_connection((ip, 4403), timeout=0.5)
        s.close()
        return ip
    except Exception:
        return None

with ThreadPoolExecutor(max_workers=64) as ex:
    results = [r for r in ex.map(check, [f"192.168.31.{i}" for i in range(1, 255)]) if r]
    for r in results:
        print(f"FOUND: {r}", flush=True)
    if not results:
        print("NO_HOSTS_FOUND", flush=True)
