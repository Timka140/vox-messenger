import serial
import sys
import time

PORT = "COM6"
BAUD = 115200
DURATION = 20

print(f"reading {PORT} @ {BAUD} for {DURATION}s ...", flush=True)
try:
    ser = serial.Serial(PORT, BAUD, timeout=1)
except Exception as e:
    print(f"FAILED: {e}", flush=True)
    sys.exit(1)

wifi_keywords = ["wifi", "WiFi", "WIFI", "SSID", "ssid", "IP", "ip addr", "connect", "DHCP", "dhcp", "assoc", "AUTH", "auth", "fail", "FAIL", "error", "ERROR"]

start = time.time()
seen_lines = []
while time.time() - start < DURATION:
    try:
        line = ser.readline().decode("utf-8", errors="ignore").strip()
    except Exception:
        continue
    if not line:
        continue
    seen_lines.append(line)
    if any(k in line for k in wifi_keywords):
        print(f"[MATCH] {line}", flush=True)

ser.close()
print(f"--- total lines: {len(seen_lines)} ---", flush=True)
print("--- last 30 lines ---", flush=True)
for l in seen_lines[-30:]:
    print(l, flush=True)
