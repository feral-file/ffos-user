#!/bin/bash

# Path to the configuration file
CONFIG_FILE="/home/feralfile/vmagent/scrape.yml"

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
if [ ! -f "$CONFIG_FILE" ]; then
    echo "Error: Configuration file $CONFIG_FILE not found!"
    exit 1
fi

# Create a temporary file
TEMP_FILE=$(mktemp)

# Perform the replacement using sed
sed "s/\${JOB_NAME}/$job_name/g; s/\${FF_DEVICE_ID}/$device_id/g" "$CONFIG_FILE" > "$TEMP_FILE"

# Check if sed command was successful
if [ $? -ne 0 ]; then
    echo "Error: Failed to process the configuration file!"
    rm -f "$TEMP_FILE"
    exit 1
fi

# Move the temporary file to replace the original
mv "$TEMP_FILE" "$CONFIG_FILE"

echo "Successfully updated $CONFIG_FILE with JOB_NAME=$job_name and FF_DEVICE_ID=$device_id"