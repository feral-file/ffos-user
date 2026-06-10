#!/bin/bash
set -e

echo "=== Running feral-setupd verification in Docker ==="

# Use the same environment as CI: ubuntu-latest with Rust 1.88.0
docker run --rm \
  -v "$(pwd):/workspace" \
  -w /workspace \
  ubuntu:latest \
  bash -c "
    set -e

    echo '>>> Installing system dependencies...'
    apt-get update -qq
    apt-get install -y -qq libdbus-1-dev pkg-config curl build-essential libssl-dev > /dev/null 2>&1

    echo '>>> Installing Rust 1.88.0...'
    curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain 1.88.0 > /dev/null 2>&1
    . \$HOME/.cargo/env

    echo '>>> Running cargo fmt check...'
    cd components/feral-setupd
    cargo fmt -- --check

    echo '>>> Running cargo clippy...'
    cargo clippy --all-targets --all-features -- -D warnings

    echo '>>> Running cargo check...'
    cargo check --all-targets --all-features

    echo '>>> Running cargo test...'
    cargo test --all-targets --all-features

    echo '✓ All checks passed!'
  "
