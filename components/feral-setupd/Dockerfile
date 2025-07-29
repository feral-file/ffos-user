# x86‑64 Arch Linux under QEMU (runs on Apple Silicon)
# syntax=docker/dockerfile:1
FROM --platform=linux/amd64 archlinux:latest

ENV RUSTUP_HOME=/usr/local/rustup
ENV CARGO_HOME=/usr/local/cargo
ENV PATH=/usr/local/cargo/bin:$PATH
ENV PKGCTL_DISABLE_SYSTEMD_SANDBOXING=1

# ---- bootstrap pacman keyring (sometimes needed on fresh Arch-ARM roots) ----
RUN pacman-key --init && pacman-key --populate archlinux

# ---- base tool-chain --------------------------------------------------------
RUN pacman -Sy --noconfirm --needed \
        base-devel git clang pkgconf cmake gdb lldb \
        go rustup && \
    rustup install stable && \
    rustup default  stable && \
    rustup component add clippy rustfmt && \
    yes | pacman -Scc           # shrink layer

WORKDIR /workspace
CMD ["/bin/bash"]