#!/bin/bash
set -euo pipefail

clear

echo "🔧 Choose the testing duration:"
select choice in "1 min" "1 hr" "3 hrs" "24 hrs" "forever"; do
  case $REPLY in
    1) DURATION_SECONDS=$((1 * 60)); break ;;
    2) DURATION_SECONDS=$((1 * 60 * 60)); break ;;
    3) DURATION_SECONDS=$((3 * 60 * 60)); break ;;
    4) DURATION_SECONDS=$((24 * 60 * 60)); break ;;
    5) DURATION_SECONDS=0; break ;;
    *) echo "Please input valid option (1-5)";;
  esac
done

# Generate timestamp: e.g., 20250701T140522
TIMESTAMP=$(date +%Y%m%dT%H%M%S)
LOG_FILE="/home/soaktest/run_results/cpu_temp_log_${TIMESTAMP}.csv"

# Launch soak test (duration + timestamp)
cage -s /home/soaktest/scripts/test.sh -- "$DURATION_SECONDS" "$TIMESTAMP" "file:///home/soaktest/files/CRAWL_MULTI_LEVEL/index.html"

/home/soaktest/scripts/copy_soak_test_logs.sh $TIMESTAMP
