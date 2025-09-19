#!/usr/bin/env bash
# download-vmagent.sh
set -euo pipefail

VM_VER="${VM_VER:-v1.125.1}"
WORKDIR="${HOME}/vmagent"
VMUTILS_DIR="${WORKDIR}/vmutils/${VM_VER}"
TARBALL="vmutils-linux-amd64-${VM_VER}.tar.gz"
URL="https://github.com/VictoriaMetrics/VictoriaMetrics/releases/download/${VM_VER}/${TARBALL}"
FF_DEVICE_ID="${FF_DEVICE_ID:-$( if [[ -r /etc/hostname ]]; then tr -d $'\r\n' < /etc/hostname; else hostname; fi )}"
JOB_NAME="${JOB_NAME:-ff1-device}"

mkdir -p "${VMUTILS_DIR}" "${WORKDIR}/queue"
cd "${WORKDIR}"

# Fetch & extract once
if [[ ! -x "${WORKDIR}/vmagent-prod" ]]; then
  [[ -f "${VMUTILS_DIR}/${TARBALL}" ]] || {
    echo "Downloading ${URL} ..."
    curl -fL -o "${VMUTILS_DIR}/${TARBALL}" "${URL}"
  }
  if ! find "${VMUTILS_DIR}" -type f -name 'vmagent-prod' -perm -111 | grep -q . ; then
    echo "Extracting to ${VMUTILS_DIR} ..."
    tar -xzf "${VMUTILS_DIR}/${TARBALL}" -C "${VMUTILS_DIR}"
  fi
  BIN="$(find "${VMUTILS_DIR}" -type f -name 'vmagent-prod' -perm -111 | head -n1 || true)"
  [[ -n "${BIN}" ]] || { echo "Error: vmagent-prod not found in ${VMUTILS_DIR}"; exit 1; }
  ln -sf "${BIN}" "${WORKDIR}/vmagent-prod"
  chmod +x "${WORKDIR}/vmagent-prod"
fi

# Minimal Prometheus-style scrape config
cat > "${WORKDIR}/scrape.yml" <<YAML
global:
  scrape_interval: 15s
scrape_configs:
  - job_name: "${JOB_NAME}"
    static_configs:
      - targets:
          - "127.0.0.1:9001" # feral-sys-monitord
        labels:
          instance: "${FF_DEVICE_ID}"
    metric_relabel_configs:
      - source_labels: ["__name__"]
        regex: "^promhttp_.*"
        action: drop
YAML

echo "vmagent ready in ${WORKDIR}. Use ./vmagent-run.sh to start."
