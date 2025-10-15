#!/usr/bin/env python3
from http.server import BaseHTTPRequestHandler, HTTPServer
import json, datetime, os, threading, time, subprocess, sys, glob

SAMPLE_INTERVAL_SECONDS = 5
FLUSH_INTERVAL_SECONDS = 15
CSV_FILE = None
HTML_FILE = "/home/soaktest/scripts/temp_viewer.html"

def detect_cpu_type():
    """Detect CPU type by reading /proc/cpuinfo."""
    try:
        with open("/proc/cpuinfo", "r") as f:
            for line in f:
                line = line.lower()
                if "vendor_id" in line:
                    if "genuineintel" in line:
                        return "intel"
                    elif "authenticamd" in line:
                        return "amd"
    except Exception as e:
        print(f"[WARN] Failed to detect CPU type: {e}")
    return "intel"  # Default to Intel if detection fails

def get_cpu_temp(cpu_type):
    """Get CPU temperature based on CPU type."""
    try:
        output = subprocess.check_output(["sensors", "-u"], encoding="utf-8")
        lines = output.splitlines()
        
        if cpu_type == "intel":
            in_package = False
            for line in lines:
                if "Package id 0" in line:
                    in_package = True
                    continue
                if in_package and "temp1_input:" in line:
                    return float(line.strip().split(":")[1])
                if in_package and line.strip() == "":
                    in_package = False
        elif cpu_type == "amd":
            in_k10temp = False
            for line in lines:
                if line.startswith("k10temp-pci-"):
                    in_k10temp = True
                    continue
                if in_k10temp and "temp1_input:" in line:
                    return float(line.strip().split(":")[1])
                if in_k10temp and line.strip() == "":
                    in_k10temp = False
    except Exception as e:
        print(f"[WARN] Failed to get CPU temperature: {e}")
    return 0.0

def get_cpu_frequencies():
    """Get average CPU frequency in MHz."""
    try:
        cur_freq_paths = glob.glob("/sys/devices/system/cpu/cpu*/cpufreq/scaling_cur_freq")
        if not cur_freq_paths:
            return None

        cur_sum = 0
        for path in cur_freq_paths:
            with open(path, 'r') as f:
                cur_sum += int(f.read().strip())
        current_mhz = cur_sum / len(cur_freq_paths) / 1000.0  # kHz → MHz
        return round(current_mhz, 1)
    except Exception as e:
        print(f"[WARN] Failed to get CPU frequencies: {e}")
        return None

def get_screen_info():
    """Get screen resolution and refresh rate."""
    try:
        output = subprocess.check_output(["wlr-randr"], encoding="utf-8")
        for line in output.splitlines():
            if "current" in line:
                fields = line.strip().split()
                if len(fields) >= 3:
                    dimensions = fields[0].split("x")
                    if len(dimensions) == 2:
                        width = int(dimensions[0])
                        height = int(dimensions[1])
                        refresh_rate = float(fields[2])
                        return {
                            "width": width,
                            "height": height,
                            "refresh_rate": refresh_rate
                        }
    except Exception as e:
        print(f"[WARN] Failed to get screen info: {e}")
    return {
        "width": None,
        "height": None,
        "refresh_rate": None
    }

def background_logger(csv_path, cpu_type):
    """Background thread to log system metrics to CSV."""
    print(f"[INFO] Logger started → writing to: {csv_path}")
    last_flush_time = time.time()

    first_time = not os.path.exists(csv_path)
    with open(csv_path, "a", buffering=1) as f:
        if first_time:
            f.write("timestamp,cpu_temp_celsius,cpu_freq_mhz,width,height,refresh_rate\n")

        while True:
            timestamp = datetime.datetime.now().strftime("%Y-%m-%d %H:%M:%S")
            temp = get_cpu_temp(cpu_type)
            screen = get_screen_info()
            freq = get_cpu_frequencies()

            width = screen["width"] if screen["width"] is not None else ""
            height = screen["height"] if screen["height"] is not None else ""
            refresh = screen["refresh_rate"] if screen["refresh_rate"] is not None else ""

            f.write(f"{timestamp},{temp:.1f},{freq:.0f},{width},{height},{refresh}\n")

            now = time.time()
            if now - last_flush_time >= FLUSH_INTERVAL_SECONDS:
                f.flush()
                os.fsync(f.fileno())
                last_flush_time = now

            time.sleep(SAMPLE_INTERVAL_SECONDS)

class TempHandler(BaseHTTPRequestHandler):
    def _send_json(self, payload):
        self.send_response(200)
        self.send_header('Content-type', 'application/json')
        self.send_header('Access-Control-Allow-Origin', '*')
        self.end_headers()
        self.wfile.write(json.dumps(payload).encode())

    def _send_html(self, path):
        try:
            with open(path, 'rb') as f:
                content = f.read()
            self.send_response(200)
            self.send_header('Content-type', 'text/html')
            self.send_header('Access-Control-Allow-Origin', '*')
            self.end_headers()
            self.wfile.write(content)
        except Exception as e:
            print(f"[WARN] Failed to serve HTML: {e}")
            self.send_response(404)
            self.end_headers()

    def do_GET(self):
        if self.path == '/info':
            try:
                with open(CSV_FILE, 'r') as f:
                    last = f.readlines()[-1]
                    parts = last.strip().split(',')
                    timestamp, temp, freq, width, height, refresh = parts
                    screen_info = {
                        "width": int(width) if width else None,
                        "height": int(height) if height else None,
                        "refresh_rate": float(refresh) if refresh else None
                    }
            except Exception as e:
                print(f"[WARN] Failed to read CSV: {e}")
                timestamp, temp, freq = '', 'N/A', 'N/A'
                screen_info = get_screen_info()

            payload = {
                'timestamp': timestamp,
                'temp': temp,
                'freq': freq,
                'screen': screen_info
            }
            self._send_json(payload)
        elif self.path in ['/', '/index.html']:
            self._send_html(HTML_FILE)
        else:
            self.send_response(404)
            self.end_headers()

def run():
    global CSV_FILE

    if len(sys.argv) < 2:
        print("Usage: ./temp_server.py <timestamp>")
        print("Example: ./temp_server.py 20250701T130000")
        sys.exit(1)

    timestamp = sys.argv[1]
    CSV_FILE = f"/home/soaktest/run_results/cpu_temp_log_{timestamp}.csv"
    cpu_type = detect_cpu_type()
    print(f"[INFO] Detected CPU type: {cpu_type}")

    threading.Thread(target=background_logger, args=(CSV_FILE, cpu_type), daemon=True).start()
    server = HTTPServer(('', 8000), TempHandler)
    print(f"[INFO] Server started at http://localhost:8000")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("[INFO] Server shutting down")
        server.server_close()

if __name__ == '__main__':
    run()