#!/bin/bash
set -euo pipefail

sudo chmod 755 /home/soaktest/scripts/soak-test.sh
sudo chmod 755 /home/soaktest/scripts/test.sh
sudo chmod 755 /home/soaktest/scripts/summary.py
sudo chmod +x /usr/local/bin/websocat

RESULTS_JSON_FILE="soak_results.json"
SUMMARY_SCRIPT_PATH="/home/soaktest/scripts/summary.py" # Path to your Python summary script

RED=$'\e[0;31m'
YEL=$'\e[1;33m'
GRN=$'\e[0;32m'
BLU=$'\e[0;34m'
NC=$'\e[0m'

log_info() { echo -e "${BLU}[INFO] $(date '+%Y-%m-%d %H:%M:%S') $1${NC}"; }
log_warn() { echo -e "${YEL}[WARN] $(date '+%Y-%m-%d %H:%M:%S') $1${NC}"; }
log_error() { echo -e "${RED}[ERROR] $(date '+%Y-%m-%d %H:%M:%S') $1${NC}"; }

cage -s /home/soaktest/scripts/test.sh 2>&1 | tee -a /home/soaktest/logs/log.log

clear

# --- Call Python Summary Script ---
if [ -f "$SUMMARY_SCRIPT_PATH" ] && command -v python3 &>/dev/null; then
log_info "Generating summary report using $SUMMARY_SCRIPT_PATH..."
python3 "$SUMMARY_SCRIPT_PATH" "$RESULTS_JSON_FILE"
else
log_warn "Could not generate summary. Python3 or $SUMMARY_SCRIPT_PATH not found."
log_info "Results are saved in $RESULTS_JSON_FILE"
fi

# --- Prompt for Shutdown ---
echo ""
read -n 1 -s -r -p "All tests finished. Results saved to $RESULTS_JSON_FILE. Press any key to shutdown the system..."
echo # Newline after keypress
log_info "Shutting down system now..."
sudo shutdown -h now