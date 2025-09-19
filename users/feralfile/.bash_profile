sudo chown -R feralfile:feralfile /home/feralfile

if [ "$(tty)" = "/dev/tty1" ]; then
    chmod +x /home/feralfile/.file_permissions.sh
    chmod +x /home/feralfile/.start-services.sh

    ~/.file_permissions.sh
    ~/.start-services.sh
fi