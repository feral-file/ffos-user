FROM ubuntu:24.04@sha256:73b999393d07f2bf65a40373778a44dc9a6d54ad7718d29247d8ff9413c4d6a8
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    build-essential cmake ninja-build pkg-config python3 ca-certificates curl \
    libsqlite3-dev libsdl2-dev libegl1-mesa-dev libgles2-mesa-dev libdrm-dev \
    libcairo2-dev libpango1.0-dev libqrencode-dev libnss3 libatk-bridge2.0-0 \
    libxcomposite1 libxdamage1 libxrandr2 libgbm1 libasound2t64 libgtk-3-0 \
    && dpkg-query -W > /toolchain-packages.txt && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    meson git python3-jinja2 python3-venv nasm libgnutls28-dev libcurl4-openssl-dev libjpeg-dev wayland-protocols libavcodec-dev libavformat-dev libavfilter-dev \
    libavutil-dev libswresample-dev libswscale-dev libass-dev libva-dev \
    libpulse-dev liblcms2-dev libdisplay-info-dev libxxhash-dev libgl-dev libwayland-dev \
    && dpkg-query -W > /toolchain-packages.txt && rm -rf /var/lib/apt/lists/*

RUN python3 -m venv /opt/build-tools && /opt/build-tools/bin/pip install --no-cache-dir meson==1.11.2 Jinja2==3.1.6 MarkupSafe==3.0.3
ENV PATH="/opt/build-tools/bin:${PATH}"
