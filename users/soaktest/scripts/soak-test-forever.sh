#!/bin/bash
set -euo pipefail

clear

# Generate timestamp: e.g., 20250701T140522
TIMESTAMP=$(date +%Y%m%dT%H%M%S)
LOG_FILE="/home/soaktest/run_results/cpu_temp_log_${TIMESTAMP}.csv"

# Launch soak test (duration + timestamp)
cage -s /home/soaktest/scripts/test.sh -- "0" "$TIMESTAMP" "file:///home/soaktest/files/CRAWL_MULTI_LEVEL/index.html"

/home/soaktest/scripts/copy_soak_test_logs.sh $TIMESTAMP
