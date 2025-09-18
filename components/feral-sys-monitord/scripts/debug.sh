go build -o feral-sys-monitord
if [ $? -ne 0 ]; then
    echo "go build failed, stopping script"
    exit 1
fi
systemctl --user stop feral-sys-monitord.service && \
sudo cp ./feral-sys-monitord /usr/bin/feral-sys-monitord && \
systemctl --user daemon-reload && \
systemctl --user start feral-sys-monitord.service && \
tail -n 20 -f ~/.logs/sys-monitord.log