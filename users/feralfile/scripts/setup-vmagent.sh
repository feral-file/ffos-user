#!/bin/bash

# Path to the configuration file
SCRAPE_FILE="/home/feralfile/vmagent/scrape.yml"
CONFIG_FILE="/home/feralfile/ff1-config.json"

# Check if the JSON config file exists
if [ ! -f "$CONFIG_FILE" ]; then
    echo "Error: Configuration file $CONFIG_FILE not found!"
    exit 1
fi

# Read BRANCH and VERSION from ff1-config.json
BRANCH=$(jq -r '.branch' "$CONFIG_FILE" 2>/dev/null)
VERSION=$(jq -r '.version' "$CONFIG_FILE" 2>/dev/null)

# Check if BRANCH and VERSION are non-empty
if [ -z "$BRANCH" ]; then
    echo "Error: Failed to read 'branch' from $CONFIG_FILE or it is empty!"
    exit 1
fi
if [ -z "$VERSION" ]; then
    echo "Error: Failed to read 'version' from $CONFIG_FILE or it is empty!"
    exit 1
fi

# Hardcode JOB_NAME
job_name="ff1-device"

# Read FF_DEVICE_ID from /etc/hostname
if [ ! -f "/etc/hostname" ]; then
    echo "Error: /etc/hostname file not found!"
    exit 1
fi

device_id=$(cat /etc/hostname)
if [ -z "$device_id" ]; then
    echo "Error: FF_DEVICE_ID read from /etc/hostname is empty!"
    exit 1
fi

# Check if the configuration file exists
if [ ! -f "$SCRAPE_FILE" ]; then
    echo "Error: Configuration file $SCRAPE_FILE not found!"
    exit 1
fi

# Create a temporary file
TEMP_FILE=$(mktemp)
if [ $? -ne 0 ]; then
    echo "Error: Failed to create temporary file!"
    exit 1
fi

# Sanitize variables by escaping special characters for sed
job_name_esc=$(printf '%s' "$job_name" | sed 's/[\/&]/\\&/g')
device_id_esc=$(printf '%s' "$device_id" | sed 's/[\/&]/\\&/g')
VERSION_esc=$(printf '%s' "$VERSION" | sed 's/[\/&]/\\&/g')
BRANCH_esc=$(printf '%s' "$BRANCH" | sed 's/[\/&]/\\&/g')

# Perform the replacement using sed with # as delimiter
sed "s#\${JOB_NAME}#$job_name_esc#g;s#\${FF_DEVICE_ID}#$device_id_esc#g;s#\${FF_VERSION}#$VERSION_esc#g;s#\${FF_BRANCH}#$BRANCH_esc#g" "$SCRAPE_FILE" > "$TEMP_FILE"

# Check if sed command was successful
if [ $? -ne 0 ]; then
    echo "Error: Failed to process the configuration file $SCRAPE_FILE!"
    rm -f "$TEMP_FILE"
    exit 1
fi

# Move the temporary file to replace the original
mv "$TEMP_FILE" "$SCRAPE_FILE"
if [ $? -ne 0 ]; then
    echo "Error: Failed to move $TEMP_FILE to $SCRAPE_FILE!"
    rm -f "$TEMP_FILE"
    exit 1
fi

echo "Successfully updated $SCRAPE_FILE with JOB_NAME=$job_name, FF_DEVICE_ID=$device_id, VERSION=$VERSION, BRANCH=$BRANCH"