# DDC/CI Display Control Setup (ddcutil)

This document describes how to configure the system to allow `ddcutil` commands to control monitor power and settings without requiring password prompts.

## Prerequisites

- `ddcutil` installed: `sudo pacman -S ddcutil`
- Monitor that supports DDC/CI (most modern displays do)
- Connected via DisplayPort or HDMI

## Option 1: Udev Rules + I2C Group (Recommended)

This approach grants permission via Linux device groups.

### 1. Load i2c-dev kernel module

```bash
echo i2c-dev | sudo tee /etc/modules-load.d/i2c-dev.conf
sudo modprobe i2c-dev
```

### 2. Create i2c group and add user

```bash
sudo groupadd --system i2c
sudo usermod -aG i2c feralfile
```

### 3. Install udev rules

```bash
sudo cp system-config/45-ddcutil-i2c.rules /etc/udev/rules.d/
sudo udevadm control --reload-rules
sudo udevadm trigger
```

### 4. Reboot or re-login

```bash
# Either reboot
sudo reboot

# Or logout and login again for group membership to take effect
```

### 5. Verify

```bash
# Should work without sudo now
ddcutil detect
ddcutil getvcp D6  # Get power state
```

## Option 2: Passwordless Sudo (Fallback)

If Option 1 doesn't work or you need a quick solution:

### 1. Validate and install sudoers file

```bash
# Validate syntax first (important!)
sudo visudo -c -f system-config/ddcutil-sudoers

# If validation passes, install it
sudo cp system-config/ddcutil-sudoers /etc/sudoers.d/ddcutil
sudo chmod 0440 /etc/sudoers.d/ddcutil
```

### 2. Verify

```bash
# Should not prompt for password
sudo ddcutil detect
```

## Troubleshooting

### Check if your monitor supports DDC/CI

```bash
ddcutil detect
```

If no displays are detected, your monitor may not support DDC/CI or the connection type (e.g., VGA) doesn't support it.

### Check i2c devices

```bash
ls -l /dev/i2c-*
```

Should show devices owned by `i2c` group (Option 1) or accessible to your user.

### Check group membership

```bash
groups feralfile
```

Should include `i2c` if using Option 1.

### Test without code

```bash
# Turn off display
ddcutil setvcp D6 02

# Turn on display
ddcutil setvcp D6 01
```

## VCP Code Reference

- `D6 02` = Power off (standby)
- `D6 01` = Power on
- `D6 04` = Power off (hard off, if supported)

## Notes

- The `--noverify` flag in the code skips DDC verification for faster execution
- DDC commands can take 50-200ms to execute
- Some monitors may take additional time to actually power on/off after the command
