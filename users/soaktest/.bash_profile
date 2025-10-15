# fix for screen readers
if grep -Fqa 'accessibility=' /proc/cmdline &> /dev/null; then
    setopt SINGLE_LINE_ZLE
fi

sudo chown -R soaktest:soaktest /home/soaktest

chmod +x /home/soaktest/.automated_script.sh
chmod +x /home/soaktest/.file_permissions.sh

~/.file_permissions.sh
~/.automated_script.sh
