cargo build --release
if [ $? -ne 0 ]; then
    echo "cargo build failed, stopping script"
    exit 1
fi
sudo systemctl stop feral-setupd.service
sudo cp ~/feral-setupd/target/release/feral-setupd /usr/bin/feral-setupd
sudo systemctl daemon-reload
sudo systemctl start feral-setupd.service
tail -n 20 -f ~/.logs/setupd.log