import json
import sys
from collections import defaultdict

# Attempt to use Rich for pretty printing, fallback to ANSI otherwise
try:
    from rich.console import Console
    from rich.table import Table
    from rich.text import Text
    from rich.style import Style
    from rich.padding import Padding
    from rich.align import Align
    USE_RICH = True
except ImportError:
    USE_RICH = False

# ANSI Colors (fallback if Rich is not available)
class AnsiColors:
    HEADER = '\033[95m'
    BLUE = '\033[94m'
    CYAN = '\033[96m'
    GREEN = '\033[92m'
    YELLOW = '\033[93m'
    RED = '\033[91m'
    ENDC = '\033[0m'
    BOLD = '\033[1m'
    UNDERLINE = '\033[4m'

# Mapping of full file URLs to user-friendly names
FILE_NAME_MAP = {
    "file:///home/soaktest/files/36-point/index.html?edition_number=0&artwork_number=1&blockchain=bitmark#02_hex_hole_open": "36-point",
    "file:///home/soaktest/files/e-volved-formula-23/index.html": "E-volved-formula-23",
    "file:///home/soaktest/files/TransparentGrit/index.html": "TransparentGrit",
    "file:///home/soaktest/files/uneasy-dream/index.html": "Uneasy-dream",
    "file:///home/soaktest/files/autoplay_10bits.html": "MoMA Unsupervised 4K H.265 10 bits",
    "file:///home/soaktest/files/autoplay_8bits.html": "MoMA Unsupervised 4K H.265 8 bits",
}

def get_color_for_value(value, high_is_bad=True, green_thresh=30, yellow_thresh=70):
    """Returns an ANSI color code based on value thresholds."""
    if USE_RICH: # Rich handles styling itself, this is for direct ANSI
        return "" # No prefix for rich
        
    try:
        val = float(value)
        if high_is_bad:
            if val > yellow_thresh: return AnsiColors.RED
            if val > green_thresh: return AnsiColors.YELLOW
            return AnsiColors.GREEN
        else: # Low is bad (e.g., FPS)
            if val < green_thresh: return AnsiColors.RED # e.g. FPS < 30 is red
            if val < yellow_thresh: return AnsiColors.YELLOW # e.g. FPS < 50 is yellow
            return AnsiColors.GREEN
    except ValueError:
        return "" # Default no color

def format_value_rich(value, unit="", high_is_bad=True, green_thresh=30, yellow_thresh=70, good_fps_thresh=55, mid_fps_thresh=45):
    """Formats value with Rich styling."""
    text_val = f"{value:.1f}{unit}" if isinstance(value, (float)) else f"{value}{unit}"
    try:
        val_f = float(value)
        style = Style()
        if unit == "FPS": # Special handling for FPS
             if val_f >= good_fps_thresh: style = Style(color="green")
             elif val_f >= mid_fps_thresh: style = Style(color="yellow")
             else: style = Style(color="red")
        elif unit == "°C": # Temperature
            if val_f > 75: style = Style(color="red")
            elif val_f > 60: style = Style(color="yellow")
            else: style = Style(color="green")
        elif "%" in unit: # Percentages
            if high_is_bad:
                if val_f > yellow_thresh: style = Style(color="red")
                elif val_f > green_thresh: style = Style(color="yellow")
                else: style = Style(color="green")
            else: # Low is bad
                if val_f < green_thresh: style = Style(color="red")
                elif val_f < yellow_thresh: style = Style(color="yellow")
                else: style = Style(color="green")
        return Text(text_val, style=style)
    except ValueError:
        return Text(str(value) + unit)

def format_duration(seconds):
    seconds = int(seconds)
    d = seconds // 86400
    h = (seconds % 86400) // 3600
    m = (seconds % 3600) // 60
    s = seconds % 60
    if d > 0:
        return f"{d}d {h}h {m}m"
    elif h > 0:
        return f"{h}h {m}m"
    elif m > 0:
        return f"{m}m {s}s"
    else:
        return f"{s}s"

def main_summary(json_file_path):
    try:
        with open(json_file_path, 'r') as f:
            results = json.load(f)
    except FileNotFoundError:
        print(f"{AnsiColors.RED if not USE_RICH else ''}Error: Results file '{json_file_path}' not found.{AnsiColors.ENDC if not USE_RICH else ''}")
        return
    except json.JSONDecodeError:
        print(f"{AnsiColors.RED if not USE_RICH else ''}Error: Could not decode JSON from '{json_file_path}'.{AnsiColors.ENDC if not USE_RICH else ''}")
        return

    if not results:
        print(f"{AnsiColors.YELLOW if not USE_RICH else ''}No results found in '{json_file_path}'.{AnsiColors.ENDC if not USE_RICH else ''}")
        return

    # Define which metrics to display and how
    # (key_in_json, display_name, unit, high_is_bad, green_thresh, yellow_thresh, (good_fps, mid_fps for FPS))
    metric_display_info = [
        ("file_url_short", "File", "", True, 0, 0), # Special handling for file name
        ("avg_cpu_chromium_pct", "CPU Chr", "%", True, 30, 70),
        ("avg_cpu_system_pct", "CPU Sys", "%", True, 40, 80),
        ("avg_cpu_temp_c", "CPU Temp", "°C", True, 60, 75),
        ("avg_cpu_freq_mhz", "CPU Freq", "MHz", False, 0, 0), # No color coding for freq generally
        ("avg_fps", "Avg FPS", "FPS", False, 30, 50, 55, 45), # low_is_bad, red_thresh, yellow_thresh, (good_fps, mid_fps)
        ("one_pct_low_fps", "1% Low FPS", "FPS", False, 25, 45, 50, 40),
        ("avg_chrome_mem_mb", "Chr Mem", "MB", True, 0, 0), # No color coding, just info
        ("avg_dropped_frames_pct", "Drop Frame", "%", True, 5, 15),
        ("failures_cpu_overheat", "CPU Fail", "", True, 0, 0.5), # 0 is green, >0 is red
        ("failures_chromium_unresponsive", "Chr Fail", "", True, 0, 0.5),
        ("actual_duration_pretty", "Run Time", "", False, 0, 0),
    ]
    
    # Prepare data for table and overall average calculation
    all_files_data = []
    aggregated_metrics = defaultdict(lambda: {"sum": 0.0, "count": 0, "is_failure_metric": False})

    for res in results:
        file_data = {}
        metrics = res.get("metrics", {})
        
        # Shorten file URL
        file_url_full = res.get("file_url", "Unknown File")
        file_data["file_url_short"] = FILE_NAME_MAP.get(file_url_full, file_url_full.split('/')[-1])

        file_data["avg_cpu_chromium_pct"] = metrics.get("cu", 0.0)
        file_data["avg_cpu_system_pct"] = metrics.get("su", 0.0)
        file_data["avg_cpu_temp_c"] = metrics.get("ct", 0.0)
        file_data["avg_cpu_freq_mhz"] = metrics.get("cf", 0.0)
        file_data["avg_fps"] = metrics.get("fps", 0.0)
        file_data["one_pct_low_fps"] = metrics.get("one_pct_low_fps", 0.0)
        file_data["avg_chrome_mem_mb"] = metrics.get("cm", 0.0)
        file_data["avg_dropped_frames_pct"] = metrics.get("df", 0.0)
        
        failures = res.get("failures", {})
        file_data["failures_cpu_overheat"] = failures.get("cpu_overheat", 0)
        file_data["failures_chromium_unresponsive"] = failures.get("chromium_unresponsive", 0)

        actual_dur = res.get("actual_duration_seconds", 0)
        file_data["actual_duration_pretty"] = format_duration(actual_dur)

        all_files_data.append(file_data)

        # Aggregate for overall average (only if samples were collected)
        if res.get("samples_collected", 0) > 0:
            for key, val in file_data.items():
                if key not in ["file_url_short"]: # Don't average file names
                    try:
                        float_val = float(val)
                        aggregated_metrics[key]["sum"] += float_val
                        aggregated_metrics[key]["count"] += 1
                        if "failures_" in key:
                             aggregated_metrics[key]["is_failure_metric"] = True
                    except ValueError:
                        pass # Cannot convert to float, skip for averaging

    # Calculate overall averages
    overall_avg_data = {"file_url_short": "Total Avg / Sum"}
    for key, data in aggregated_metrics.items():
        if data["count"] > 0:
            if data["is_failure_metric"]: # For failures, show sum
                 overall_avg_data[key] = data["sum"]
            else: # For other metrics, show average
                 overall_avg_data[key] = data["sum"] / data["count"]
        else:
            overall_avg_data[key] = 0.0
    
    if USE_RICH:
        console = Console()
        table = Table(title=Text("Soak Test Summary Report", style="bold magenta"), show_header=True, header_style="bold blue")
        
        for _, display_name, _, _, _, _, *_ in metric_display_info:
            table.add_column(display_name, justify="center")

        for row_data in all_files_data:
            row_values_rich = []
            for key, _, unit, high_is_bad, g_thresh, y_thresh, *fps_thresh in metric_display_info:
                val = row_data.get(key, "N/A")
                if key == "file_url_short":
                    row_values_rich.append(Text(str(val), style="cyan"))
                else:
                    good_fps = fps_thresh[0] if len(fps_thresh) > 0 else 55
                    mid_fps = fps_thresh[1] if len(fps_thresh) > 1 else 45
                    row_values_rich.append(format_value_rich(val, unit, high_is_bad, g_thresh, y_thresh, good_fps, mid_fps))
            table.add_row(*row_values_rich)
        
        # Add Overall Average Row
        if aggregated_metrics: # Only add if there's data to average
            table.add_section()
            overall_row_rich = []
            for key, _, unit, high_is_bad, g_thresh, y_thresh, *fps_thresh in metric_display_info:
                val = overall_avg_data.get(key, "N/A")
                if key == "file_url_short":
                     overall_row_rich.append(Text(str(val), style="bold cyan"))
                else:
                    good_fps = fps_thresh[0] if len(fps_thresh) > 0 else 55
                    mid_fps = fps_thresh[1] if len(fps_thresh) > 1 else 45
                    overall_row_rich.append(format_value_rich(val, unit, high_is_bad, g_thresh, y_thresh, good_fps, mid_fps))
            table.add_row(*overall_row_rich)
        
        console.print(Align.center(table))

    else: # Fallback to basic ANSI printing
        header = " | ".join([info[1] for info in metric_display_info])
        print(f"{AnsiColors.BOLD}{AnsiColors.BLUE}{header}{AnsiColors.ENDC}")
        print("-" * (len(header) + (len(metric_display_info) -1) * 3))

        for row_data in all_files_data:
            row_str_parts = []
            for key, _, unit, high_is_bad, g_thresh, y_thresh, *fps_thresh in metric_display_info:
                val = row_data.get(key, "N/A")
                color = ""
                if key == "file_url_short":
                    color = AnsiColors.CYAN
                    val_str = str(val)[:15] # Truncate file name if too long
                elif isinstance(val, (float, int)):
                    val_str = f"{val:.1f}{unit}" if isinstance(val, float) else f"{val}{unit}"
                    if unit == "FPS":
                        color = get_color_for_value(val, False, fps_thresh[0] if fps_thresh else 30, fps_thresh[1] if len(fps_thresh)>1 else 50) # Low is bad
                    else:
                        color = get_color_for_value(val, high_is_bad, g_thresh, y_thresh)
                else:
                    val_str = str(val)
                
                row_str_parts.append(f"{color}{val_str.center(len(_))}{AnsiColors.ENDC}") # Center within column width guess
            print(" | ".join(row_str_parts))

        # Print Overall Average Row (Basic)
        if aggregated_metrics:
            print("-" * (len(header) + (len(metric_display_info) -1) * 3))
            overall_row_parts = []
            for key, _, unit, high_is_bad, g_thresh, y_thresh, *fps_thresh in metric_display_info:
                val = overall_avg_data.get(key, "N/A")
                color = ""
                if key == "file_url_short":
                    color = f"{AnsiColors.BOLD}{AnsiColors.CYAN}"
                    val_str = str(val)
                elif isinstance(val, (float, int)):
                    val_str = f"{val:.1f}{unit}" if isinstance(val, float) else f"{val}{unit}"
                    if unit == "FPS":
                         color = get_color_for_value(val, False, fps_thresh[0] if fps_thresh else 30, fps_thresh[1] if len(fps_thresh)>1 else 50)
                    else:
                        color = get_color_for_value(val, high_is_bad, g_thresh, y_thresh)
                else:
                    val_str = str(val)
                overall_row_parts.append(f"{color}{val_str.center(len(_))}{AnsiColors.ENDC}")
            print(" | ".join(overall_row_parts))
    
    # Print summary of failures
    total_cpu_fails = sum(res.get("failures", {}).get("cpu_overheat", 0) for res in results)
    total_chrome_fails = sum(res.get("failures", {}).get("chromium_unresponsive", 0) for res in results)

    failure_summary_title = "\nTotal Failures Summary:"
    failure_summary_cpu = f"CPU Overheats: {total_cpu_fails}"
    failure_summary_chrome = f"Chromium Unresponsive: {total_chrome_fails}"
    if USE_RICH:
        console.print(Align.center(Text(failure_summary_title, style="bold yellow")))
        console.print(Align.center(Text(failure_summary_cpu, style="red" if total_cpu_fails > 0 else "green")))
        console.print(Align.center(Text(failure_summary_chrome, style="red" if total_chrome_fails > 0 else "green")))
    else:
        print(f"\n{AnsiColors.BOLD}{AnsiColors.YELLOW}{failure_summary_title}{AnsiColors.ENDC}")
        print(f"{AnsiColors.RED if total_cpu_fails > 0 else AnsiColors.GREEN}{failure_summary_cpu}{AnsiColors.ENDC}")
        print(f"{AnsiColors.RED if total_chrome_fails > 0 else AnsiColors.GREEN}{failure_summary_chrome}{AnsiColors.ENDC}")


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print(f"{AnsiColors.RED if not USE_RICH else ''}Usage: python3 summary.py <path_to_soak_results.json>{AnsiColors.ENDC if not USE_RICH else ''}")
        sys.exit(1)
    json_file = sys.argv[1]
    main_summary(json_file)