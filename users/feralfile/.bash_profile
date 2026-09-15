# On current images nothing logs in on tty1 (ffos#126): feral-kiosk-startup.service
# in the ffos image runs .file_permissions.sh and .start-services.sh as feralfile.
# The tty1 branch below is kept only so an image without that unit still boots
# the kiosk from a tty1 autologin.
sudo chown -R feralfile:feralfile /home/feralfile

if [ "$(tty)" = "/dev/tty1" ]; then
    chmod +x /home/feralfile/.file_permissions.sh
    chmod +x /home/feralfile/.start-services.sh

    ~/.file_permissions.sh
    ~/.start-services.sh
fi