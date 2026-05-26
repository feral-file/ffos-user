package constant

const (
	// WEBAPP_URL is the bundled local player served by feral-player.service.
	WEBAPP_URL = "http://127.0.0.1:8080/"

	FF1_CONFIG_FILE = "/home/feralfile/ff1-config.json"
	HOSTNAME_FILE   = "/etc/hostname"

	SCREEN_ORIENTATION_FILE = "/home/feralfile/.state/screen-orientation"
	STATE_FILE              = "/home/feralfile/.state/controld.state"
	SLEEP_SCHEDULE_FILE     = "/home/feralfile/.state/sleep-schedule.json"
	CONFIG_FILE             = "/home/feralfile/.config/controld.json"

	SSH_AUTHORIZED_KEYS_FILE = "/home/feralfile/.ssh/authorized_keys"
	SSH_DISABLE_UNIT         = "ff1-ssh-disable"
)

var (
	CHROMIUM_OOM_KILL_COUNT_FILE         = "/var/lib/oom_state/chromium-oom-kill-count"
	CHROMIUM_OOM_KILL_HANDLED_COUNT_FILE = "/var/lib/oom_state/chromium-oom-kill-handled-count"
)
