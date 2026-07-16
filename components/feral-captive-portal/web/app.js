const deviceLabel = document.getElementById("deviceLabel");
const hotspotPasswordLabel = document.getElementById("hotspotPasswordLabel");
const scanHint = document.getElementById("scanHint");
const networkList = document.getElementById("networkList");
const networkPanel = document.getElementById("networkPanel");
const passwordPanel = document.getElementById("passwordPanel");
const selectedTitle = document.getElementById("selectedTitle");
const connectForm = document.getElementById("connectForm");
const passwordInput = document.getElementById("passwordInput");
const refreshBtn = document.getElementById("refreshBtn");
const backBtn = document.getElementById("backBtn");
const connectBtn = document.getElementById("connectBtn");
const statusPanel = document.getElementById("statusPanel");
const statusBody = document.getElementById("statusBody");
const statusSpinner = document.getElementById("statusSpinner");
const statusMessage = document.getElementById("statusMessage");
const statusHint = document.getElementById("statusHint");
const retryBtn = document.getElementById("retryBtn");

const POLL_INTERVAL_MS = 800;
const POLL_TIMEOUT_MS = 120000;

let selectedNetwork = null;
let pollTimer = null;

async function fetchJson(path, options = {}) {
  const response = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...options,
  });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(payload.error || "Request failed");
    error.payload = payload;
    error.status = response.status;
    throw error;
  }
  return payload;
}

function stopPolling() {
  if (pollTimer) {
    clearInterval(pollTimer);
    pollTimer = null;
  }
}

function hideMainPanels() {
  networkPanel.hidden = true;
  passwordPanel.hidden = true;
}

function showMainPanels() {
  statusPanel.hidden = true;
  statusBody.hidden = true;
  networkPanel.hidden = false;
  passwordPanel.hidden = true;
}

function showStatus(message, options = {}) {
  const {
    kind = "ok",
    hint = "",
    spinning = false,
    showRetry = false,
  } = options;

  hideMainPanels();
  statusPanel.hidden = false;
  statusBody.hidden = false;
  statusPanel.classList.remove("ok", "error", "pending");
  statusPanel.classList.add(
    kind === "error" ? "error" : kind === "pending" ? "pending" : "ok",
  );
  statusSpinner.hidden = !spinning;
  statusMessage.textContent = message;
  statusHint.hidden = !hint;
  statusHint.textContent = hint;
  retryBtn.hidden = !showRetry;
}

function failureMessage(state) {
  const code = state.code || "";
  if (code === "wrong_password") {
    return "That password did not work. Check the password and try again.";
  }
  if (code === "network_not_found") {
    return "That network was not found. Move closer to your router and try again.";
  }
  if (code === "hotspot_restore_failed") {
    return "FF1 could not re-open setup hotspot. Reboot the device and try again.";
  }
  if (code === "timeout") {
    return "Connection timed out before FF1 could finish joining the network.";
  }
  return state.error || "FF1 could not join that network.";
}

function renderFailure(state) {
  stopPolling();
  showStatus(failureMessage(state), {
    kind: "error",
    hint:
      "Setup hotspot should be available again. You can pick the network and retry.",
    showRetry: true,
  });
}

function renderSuccess(state) {
  stopPolling();
  showStatus(`Connected to ${state.ssid}. You can return to your normal Wi-Fi network.`, {
    kind: "ok",
    hint:
      "FF1 has left hotspot mode. Join the same Wi-Fi on your phone if you need internet again.",
  });
}

function renderConnecting(state) {
  showStatus(`Connecting to ${state.ssid}…`, {
    kind: "pending",
    spinning: true,
    hint:
      "This page may disconnect briefly while FF1 joins the network. If it does, wait for the hotspot to return or join the same Wi-Fi once setup finishes.",
  });
}

async function pollConnectStatus() {
  try {
    const state = await fetchJson("/api/connect/status");
    if (state.phase === "connecting") {
      renderConnecting(state);
      return;
    }
    if (state.phase === "success") {
      renderSuccess(state);
      return;
    }
    if (state.phase === "failed") {
      renderFailure(state);
      return;
    }
    stopPolling();
  } catch (_error) {
    // Hotspot is often down during nmcli join; keep showing the pending state.
  }
}

function startPolling() {
  stopPolling();
  pollConnectStatus();
  pollTimer = setInterval(pollConnectStatus, POLL_INTERVAL_MS);
  setTimeout(() => {
    if (!pollTimer) {
      return;
    }
    stopPolling();
    showStatus("Still working. If this page disconnected, reopen the setup portal.", {
      kind: "pending",
      spinning: true,
      hint:
        "When hotspot returns, this page will show whether the password worked.",
    });
  }, POLL_TIMEOUT_MS);
}

function renderNetworks(networks) {
  networkList.innerHTML = "";
  if (!networks.length) {
    scanHint.textContent = "No networks found. Move closer to your router and refresh.";
    networkList.hidden = true;
    return;
  }

  scanHint.textContent = "Tap a network to continue.";
  networkList.hidden = false;

  for (const network of networks) {
    const item = document.createElement("button");
    item.type = "button";
    item.className = "network-item";
    item.innerHTML = `
      <span class="network-meta">
        <strong>${escapeHtml(network.ssid)}</strong>
        <span>${network.secured ? "Secured" : "Open network"}</span>
      </span>
      <span class="signal">${network.signal}%</span>
    `;
    item.addEventListener("click", () => openPasswordStep(network));
    networkList.appendChild(item);
  }
}

function escapeHtml(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;");
}

function openPasswordStep(network) {
  selectedNetwork = network;
  networkPanel.hidden = true;
  passwordPanel.hidden = false;
  statusPanel.hidden = true;
  selectedTitle.textContent = network.ssid;
  passwordInput.value = "";
  passwordInput.required = network.secured;
  passwordInput.placeholder = network.secured
    ? "Enter Wi-Fi password"
    : "No password required";
  passwordInput.focus();
}

function backToList() {
  passwordPanel.hidden = true;
  networkPanel.hidden = false;
  selectedNetwork = null;
}

async function loadDevice() {
  const payload = await fetchJson("/api/device");
  deviceLabel.textContent = `Hotspot: ${payload.hotspot_ssid}`;
  hotspotPasswordLabel.textContent = `Hotspot password: ${payload.hotspot_password}`;
}

async function loadNetworks() {
  refreshBtn.disabled = true;
  scanHint.textContent = "Scanning nearby networks…";
  networkList.hidden = true;
  try {
    const payload = await fetchJson("/api/networks");
    renderNetworks(payload.networks || []);
  } catch (error) {
    scanHint.textContent = error.message || "Could not scan networks.";
  } finally {
    refreshBtn.disabled = false;
  }
}

async function resetConnectState() {
  await fetchJson("/api/connect/reset", { method: "POST", body: "{}" });
}

async function resumeConnectState() {
  try {
    const state = await fetchJson("/api/connect/status");
    if (state.phase === "connecting") {
      startPolling();
      return true;
    }
    if (state.phase === "failed") {
      renderFailure(state);
      return true;
    }
    if (state.phase === "success") {
      renderSuccess(state);
      return true;
    }
  } catch (_error) {
    // Portal may still be starting; fall back to the normal network list.
  }
  return false;
}

connectForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  if (!selectedNetwork) {
    return;
  }

  connectBtn.disabled = true;
  hideMainPanels();
  renderConnecting({ ssid: selectedNetwork.ssid });

  try {
    await fetchJson("/api/connect", {
      method: "POST",
      body: JSON.stringify({
        ssid: selectedNetwork.ssid,
        password: passwordInput.value,
      }),
    });
    startPolling();
  } catch (error) {
    if (error.status === 409) {
      startPolling();
      return;
    }
    showStatus(error.message || "Could not start connection.", {
      kind: "error",
      showRetry: true,
    });
    passwordPanel.hidden = false;
  } finally {
    connectBtn.disabled = false;
  }
});

retryBtn.addEventListener("click", async () => {
  retryBtn.disabled = true;
  try {
    await resetConnectState();
    stopPolling();
    showMainPanels();
    await loadNetworks();
  } catch (error) {
    showStatus(error.message || "Could not reset setup state.", {
      kind: "error",
      showRetry: true,
    });
  } finally {
    retryBtn.disabled = false;
  }
});

refreshBtn.addEventListener("click", loadNetworks);
backBtn.addEventListener("click", backToList);

async function boot() {
  loadDevice().catch(() => {
    deviceLabel.textContent = "Hotspot: FF1";
  });

  const resumed = await resumeConnectState();
  if (!resumed) {
    await loadNetworks();
  }
}

boot();
