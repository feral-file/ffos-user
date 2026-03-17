package constant

const (
	FF1_CONFIG_FILE = "/home/feralfile/ff1-config.json"
	HOSTNAME_FILE   = "/etc/hostname"

	SCREEN_ORIENTATION_FILE = "/home/feralfile/.state/screen-orientation"
	STATE_FILE              = "/home/feralfile/.state/controld.state"
	CONFIG_FILE             = "/home/feralfile/.config/controld.json"

	SSH_AUTHORIZED_KEYS_FILE = "/home/feralfile/.ssh/authorized_keys"
	SSH_DISABLE_UNIT         = "ff1-ssh-disable"

	CHROMIUM_OOM_KILL_COUNT_FILE         = "/var/lib/oom_state/chromium-oom-kill-count"
	CHROMIUM_OOM_KILL_HANDLED_COUNT_FILE = "/var/lib/oom_state/chromium-oom-kill-handled-count"
)
