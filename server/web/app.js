"use strict";
// DroidBeam console. Talks to the same REST API as any client, using the API key the user enters.
// All server data is rendered with textContent (never innerHTML): SMS bodies come from strangers.

(() => {
  const REFRESH_MS = 5000;
  const KEY_NAME = "droidbeam.apiKey";
  const $ = (id) => document.getElementById(id);

  // ---- storage (may be unavailable in private mode) ----
  const store = {
    get() {
      try { return sessionStorage.getItem(KEY_NAME) || localStorage.getItem(KEY_NAME) || ""; } catch { return ""; }
    },
    set(key, remember) {
      try { (remember ? localStorage : sessionStorage).setItem(KEY_NAME, key); } catch { /* ignore */ }
    },
    clear() {
      try { sessionStorage.removeItem(KEY_NAME); localStorage.removeItem(KEY_NAME); } catch { /* ignore */ }
    },
  };

  let apiKey = store.get();
  let statusFilter = "";
  let timer = null;
  let devicesCache = [];

  // ---- DOM helper ----
  function h(tag, props, ...children) {
    const el = document.createElement(tag);
    if (props) {
      for (const [k, v] of Object.entries(props)) {
        if (v == null || v === false) continue;
        if (k === "class") el.className = v;
        else if (k === "text") el.textContent = v;
        else if (k === "title") el.title = v;
        else el.setAttribute(k, v);
      }
    }
    for (const c of children.flat()) {
      if (c == null || c === false) continue;
      el.append(c instanceof Node ? c : document.createTextNode(String(c)));
    }
    return el;
  }

  // ---- API ----
  class AuthError extends Error {}
  async function api(path, opts = {}) {
    const res = await fetch(path, {
      ...opts,
      headers: { "X-API-Key": apiKey, ...(opts.body ? { "Content-Type": "application/json" } : {}) },
    });
    let data = null;
    try { data = await res.json(); } catch { /* empty */ }
    if (res.status === 401) throw new AuthError("API key rejected");
    if (!res.ok) throw new Error((data && data.error) || `HTTP ${res.status}`);
    return data;
  }

  // ---- formatting ----
  const STATUS_LABEL = { queued: "Queued", sending: "Sending", sent: "Sent", delivered: "Delivered", failed: "Failed" };
  function ago(iso) {
    if (!iso) return "–";
    const s = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000));
    if (s < 5) return "just now";
    if (s < 60) return `${s}s ago`;
    if (s < 3600) return `${Math.floor(s / 60)} min ago`;
    if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
    return `${Math.floor(s / 86400)} d ago`;
  }
  function clock(iso) {
    if (!iso) return "";
    const d = new Date(iso);
    const today = new Date();
    const time = d.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit", second: "2-digit" });
    return d.toDateString() === today.toDateString() ? time : `${d.toLocaleDateString("en-GB", { day: "numeric", month: "short" })} ${time}`;
  }
  const PLUG = { ac: "charger AC", usb: "USB", wireless: "wireless", dock: "dock" };
  const BATT = { charging: "charging", discharging: "discharging", full: "full", not_charging: "plugged in, not charging", unknown: "unknown" };
  const NET = { wifi: "Wi-Fi", cellular: "Cellular", ethernet: "Ethernet", none: "No network", other: "Other" };

  // SMS segment estimate: GSM-7 = 160/153 chars, otherwise UCS-2 = 70/67.
  const GSM7 = /^[@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !"#¤%&'()*+,\-./0-9:;<=>?¡A-ZÄÖÑÜ§¿a-zäöñüà^{}\\[~\]|€]*$/;
  function segments(text) {
    const n = [...text].length;
    if (!n) return 0;
    const gsm = GSM7.test(text);
    const single = gsm ? 160 : 70, multi = gsm ? 153 : 67;
    return n <= single ? 1 : Math.ceil(n / multi);
  }

  // ---- render: stats ----
  function renderStats(stats, devices) {
    const online = devices.filter((d) => d.online).length;
    $("st-online").textContent = `${online}/${devices.length}`;
    $("st-online-foot").textContent = devices.length ? (online === devices.length ? "all connected" : `${devices.length - online} offline`) : "no devices yet";
    const t = stats.today.messages;
    const sent = (t.sent || 0) + (t.delivered || 0);
    $("st-sent").textContent = sent;
    $("st-sent-foot").textContent = `${t.delivered || 0} confirmed delivered`;
    $("st-failed").textContent = t.failed || 0;
    const pending = (t.queued || 0) + (t.sending || 0);
    $("st-failed-foot").textContent = pending ? `${pending} still queued` : "queue empty";
    $("st-inbox").textContent = stats.today.inbox;
    const beam = $("beam");
    beam.classList.toggle("is-live", online > 0);
    beam.classList.toggle("is-down", devices.length > 0 && online === 0);
  }

  // ---- render: devices ----
  function batteryBlock(b) {
    if (!b) return h("p", { class: "muted", text: "No battery data yet" });
    const lit = Math.round(b.level / 10);
    const cls = ["battery", b.level <= 20 ? "low" : b.level <= 40 ? "mid" : "", b.status === "charging" ? "charging" : ""].join(" ");
    const state = [BATT[b.status] || b.status, b.plugged && b.plugged !== "none" ? `via ${PLUG[b.plugged] || b.plugged}` : null,
      b.temperature != null ? `${b.temperature.toFixed(1)}°C` : null].filter(Boolean).join(" · ");
    const cells = h("div", { class: "cells", role: "img", "aria-label": `Battery ${b.level}%` });
    for (let i = 0; i < 10; i++) cells.append(h("span", { class: i < lit ? "cell on" : "cell" }));
    return h("div", { class: cls },
      h("div", { class: "battery-row" },
        h("span", { class: "battery-level", text: `${b.level}%` }),
        h("span", { class: "battery-state", text: state })),
      cells);
  }

  function simList(sims) {
    if (!sims || !sims.length) return null;
    return h("div", { class: "sims" }, sims.map((s) =>
      h("div", { class: `sim ${s.in_service ? "ok" : "bad"}` },
        h("span", { class: "sim-slot", text: `SIM ${s.slot}` }),
        h("span", { class: "sim-carrier", text: s.carrier || "–" }),
        s.default_sms ? h("span", { class: "tag", text: "default SMS" }) : null,
        h("span", { class: "sim-sig", text: s.in_service ? "in service" : "no service" }))));
  }

  function renderDevices(devices) {
    const box = $("devices");
    $("device-count").textContent = devices.length ? `${devices.length} registered` : "";
    if (!devices.length) {
      box.replaceChildren(h("p", { class: "empty", text: "No devices yet. Register a phone from the DroidBeam app." }));
      return;
    }
    box.replaceChildren(...devices.map((d) => {
      const i = d.info || {};
      const model = [i.manufacturer, i.model].filter(Boolean).join(" ");
      const sub = [model, i.android_version ? `Android ${i.android_version}` : null].filter(Boolean).join(" · ");
      const conn = i.connection === "fallback" ? "Cloud (fallback)" : i.connection === "primary" ? "LAN" : "–";
      return h("article", { class: `device ${d.online ? "" : "is-offline"}` },
        h("div", { class: "device-top" },
          h("div", null,
            h("div", { class: "device-name", text: d.name }),
            h("div", { class: "device-sub", text: sub || `Device #${d.id}` })),
          h("span", { class: `pulse ${d.online ? "on" : "off"}`, text: d.online ? "Online" : "Offline" })),
        batteryBlock(d.battery),
        simList(i.sims),
        h("dl", { class: "kv" },
          h("dt", { text: "Route" }), h("dd", { text: conn }),
          h("dt", { text: "Network" }), h("dd", { text: [NET[i.network] || i.network, i.operator].filter(Boolean).join(" · ") || "–" }),
          h("dt", { text: "Phone IP" }), h("dd", { text: i.local_ip || "–" }),
          h("dt", { text: "Last seen" }), h("dd", { text: ago(d.last_seen_at), title: d.last_seen_at || "" }),
          h("dt", { text: "ID / app" }), h("dd", { text: `#${d.id}${i.app_version ? ` · v${i.app_version}` : ""}` })));
    }));
  }

  function renderDeviceSelect(devices) {
    const sel = $("send-device");
    const current = sel.value;
    sel.replaceChildren(h("option", { value: "", text: "Auto" }),
      ...devices.map((d) => h("option", { value: String(d.id), text: `${d.name}${d.online ? "" : " (offline)"}` })));
    if ([...sel.options].some((o) => o.value === current)) sel.value = current;
  }

  // ---- render: messages / inbox ----
  function renderMessages(msgs) {
    const body = $("messages");
    if (!msgs.length) {
      body.replaceChildren(h("tr", null, h("td", { colspan: "6", class: "empty", text: statusFilter ? "No messages with this status" : "No messages yet" })));
      return;
    }
    body.replaceChildren(...msgs.map((m) => h("tr", null,
      h("td", { class: "mono muted", text: m.id }),
      h("td", { class: "mono nowrap", text: m.to }),
      h("td", { class: "cell-body" }, m.body, m.error ? h("span", { class: "err-line", text: m.error }) : null),
      h("td", null, h("span", { class: `pill s-${m.status}`, text: STATUS_LABEL[m.status] || m.status })),
      h("td", { class: "mono", text: m.sim_used ? `SIM ${m.sim_used}` : m.sim ? `→ SIM ${m.sim}` : "–" }),
      h("td", { class: "mono muted nowrap", text: clock(m.created_at), title: m.created_at }))));
  }

  function renderInbox(items) {
    const list = $("inbox");
    if (!items.length) {
      list.replaceChildren(h("li", { class: "empty", text: "No incoming SMS yet" }));
      return;
    }
    const names = Object.fromEntries(devicesCache.map((d) => [d.id, d.name]));
    list.replaceChildren(...items.map((m) => h("li", null,
      h("div", { class: "inbox-head" },
        h("span", { class: "inbox-from", text: m.from }),
        h("span", { class: "mono muted", text: clock(m.received_at), title: m.received_at })),
      h("div", { class: "inbox-body", text: m.body }),
      h("div", { class: "mono muted", text: [names[m.device_id] || `Device #${m.device_id}`, m.sim ? `SIM ${m.sim}` : null].filter(Boolean).join(" · ") }))));
  }

  // ---- refresh loop ----
  async function refresh() {
    try {
      const q = statusFilter ? `&status=${encodeURIComponent(statusFilter)}` : "";
      const [devices, msgs, inbox, stats] = await Promise.all([
        api("/api/v1/devices"),
        api(`/api/v1/messages?limit=50${q}`),
        api("/api/v1/inbox?limit=30"),
        api("/api/v1/stats"),
      ]);
      devicesCache = devices.data;
      renderStats(stats, devices.data);
      renderDevices(devices.data);
      renderDeviceSelect(devices.data);
      renderMessages(msgs.data);
      renderInbox(inbox.data);
      $("meta-refresh").textContent = `updated ${new Date().toLocaleTimeString("en-GB")}`;
    } catch (e) {
      if (e instanceof AuthError) return logout(e.message);
      $("meta-refresh").textContent = `failed to load: ${e.message}`;
      $("beam").classList.add("is-down");
    }
  }

  function startLoop() {
    stopLoop();
    refresh();
    timer = setInterval(() => { if (!document.hidden) refresh(); }, REFRESH_MS);
  }
  function stopLoop() { if (timer) clearInterval(timer); timer = null; }
  document.addEventListener("visibilitychange", () => { if (!document.hidden && timer) refresh(); });

  // ---- login / logout ----
  async function showApp() {
    $("login").hidden = true;
    $("app").hidden = false;
    try {
      const info = await api("/api/v1/server-info");
      $("meta-lan").textContent = info.lan_urls && info.lan_urls.length ? `LAN ${info.lan_urls.join(", ")}` : "";
    } catch (e) {
      if (e instanceof AuthError) return logout(e.message);
    }
    startLoop();
  }

  function logout(message) {
    stopLoop();
    store.clear();
    apiKey = "";
    $("app").hidden = true;
    $("login").hidden = false;
    $("login-error").textContent = message || "";
    $("login-key").value = "";
    $("login-key").focus();
  }

  $("login-form").addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const key = $("login-key").value.trim();
    if (!key) { $("login-error").textContent = "Enter the API key first"; return; }
    apiKey = key;
    try {
      await api("/api/v1/stats");
    } catch (e) {
      $("login-error").textContent = e instanceof AuthError ? "Wrong API key" : `Cannot reach server: ${e.message}`;
      apiKey = "";
      return;
    }
    store.set(key, $("login-remember").checked);
    $("login-error").textContent = "";
    showApp();
  });
  $("login-key").addEventListener("input", () => { $("login-error").textContent = ""; });
  $("logout").addEventListener("click", () => logout());

  // ---- status filter ----
  $("status-filter").addEventListener("click", (ev) => {
    const btn = ev.target.closest(".chip");
    if (!btn) return;
    statusFilter = btn.dataset.status;
    for (const c of $("status-filter").querySelectorAll(".chip")) c.classList.toggle("is-on", c === btn);
    refresh();
  });

  // ---- send form ----
  const bodyEl = $("send-body");
  function updateMeta() {
    const n = [...bodyEl.value].length;
    $("send-meta").textContent = `${n} characters · ${segments(bodyEl.value)} SMS`;
  }
  bodyEl.addEventListener("input", updateMeta);
  $("send-to").addEventListener("input", () => { $("send-result").textContent = ""; });

  $("send-form").addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const note = $("send-result");
    const to = $("send-to").value.replace(/[\s-]/g, "");
    const body = bodyEl.value;
    note.className = "form-note err";
    if (!/^\+?\d{3,20}$/.test(to)) { note.textContent = "Phone number may only contain digits (optionally starting with +)"; return; }
    if (!body.trim()) { note.textContent = "Message cannot be empty"; return; }
    const payload = { to, body };
    if ($("send-device").value) payload.device_id = Number($("send-device").value);
    if ($("send-sim").value) payload.sim = Number($("send-sim").value);

    const btn = $("send-btn");
    btn.disabled = true;
    try {
      const m = await api("/api/v1/messages", { method: "POST", body: JSON.stringify(payload) });
      note.className = "form-note ok";
      note.textContent = `Message #${m.id} queued`;
      bodyEl.value = "";
      updateMeta();
      refresh();
    } catch (e) {
      if (e instanceof AuthError) return logout(e.message);
      note.textContent = `Failed: ${e.message}`;
    } finally {
      btn.disabled = false;
    }
  });

  // ---- boot ----
  if (apiKey) showApp(); else logout();
})();
