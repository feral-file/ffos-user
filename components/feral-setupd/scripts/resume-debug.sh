cargo build
if [ $? -ne 0 ]; then
    echo "cargo build failed, stopping script"
    exit 1
fi
systemctl --user stop feral-setupd.service
sudo cp ~/feral-setupd/target/debug/feral-setupd /usr/bin/feral-setupd
systemctl --user daemon-reload
systemctl --user start feral-setupd.service
tail -n 20 -f ~/.logs/setupd.log