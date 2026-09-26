import subprocess
import socket
import re
from concurrent.futures import ThreadPoolExecutor

def ping(ip):
    try:
        subprocess.run(
            ["ping", "-n", "1", "-w", "200", ip],
            capture_output=True, timeout=2
        )
    except Exception:
        pass

print("ping sweep to populate ARP cache...", flush=True)
with ThreadPoolExecutor(max_workers=64) as ex:
    list(ex.map(ping, [f"192.168.31.{i}" for i in range(1, 255)]))

print("dumping ARP cache, looking for d8:85:ac:aa:* ...", flush=True)
out = subprocess.run(["arp", "-a"], capture_output=True, text=True, encoding="cp866", errors="ignore")
lines = out.stdout.splitlines()
for line in lines:
    low = line.lower().replace("-", ":")
    if "d8:85:ac:aa" in low:
        print(f"HIT: {line.strip()}", flush=True)

print("--- full ARP for 192.168.31.* ---", flush=True)
for line in lines:
    if "192.168.31." in line:
        print(line.strip(), flush=True)
