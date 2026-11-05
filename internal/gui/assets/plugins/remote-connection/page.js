// Local client requests and the browser login boundary. The host decides when
// this service is available; server pages must not call it.
(() => {
  const root = "/api/remote-connection";
  async function request(action, serverUrl = "") {
    const paths = { status: "", connect: "", refresh: "/refresh", disconnect: "/disconnect", manage: "/manage" };
    if (!Object.prototype.hasOwnProperty.call(paths, action)) throw new Error("remote connection request failed");
    const options = action === "status" ? {} : {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify(action === "connect" ? { serverUrl } : {}),
    };
    try {
      const response = await fetch(root + paths[action], options);
      if (!response.ok) {
        const body = await response.json().catch(() => null);
        const allowed = ["INVALID_REQUEST", "REQUEST_TOO_LARGE", "INVALID_ORIGIN", "UNAVAILABLE", "OWNER_CONFLICT", "RESTORE_FAILED", "registration_request_invalid", "tailscale_tool_unavailable", "tailscale_status_unavailable", "tailscale_mapping_unavailable", "tls_certificate_invalid", "dns_resolution_failed", "registration_timeout", "registration_unreachable", "registration_rejected", "registration_response_invalid", "provider_binding_failed", "model_refresh_failed"];
        const error = new Error("remote connection request failed");
        error.code = allowed.includes(body?.error?.code) ? body.error.code : "";
        throw error;
      }
      return await response.json();
    } catch (error) {
      if (error?.message === "remote connection request failed") throw error;
      throw new Error("remote connection request failed");
    }
  }
  function managementURL(server, candidate) {
    try {
      if (typeof server !== "string" || typeof candidate !== "string" || /[?#]/.test(server) || /[?#]/.test(candidate) || server.includes("@") || candidate.includes("@")) return null;
      const base = new URL(server), url = new URL(candidate);
      if (base.protocol !== "https:" || base.username || base.password || base.search || base.hash || base.pathname !== "/" ||
          url.protocol !== "https:" || url.origin !== base.origin || url.username || url.password || url.search || url.hash ||
          url.pathname !== "/_magpie/remote/v1/browser-login") return null;
      return url.href;
    } catch { return null; }
  }
  window.remoteConnection = { request, managementURL };
})();

// Register the page only in the explicitly designated desktop client.
if (window.bootPrefs?.remoteClient === true) {
  const nav = document.createElement("button");
  nav.dataset.view = "remote-connection";
  nav.innerHTML = '<span data-t>Remote connection: Server</span>';
  document.querySelector('#nav button[data-view="agents"]').after(nav);
  magpie.plugin("remote-connection", ctx => {
    let page, status = null, busy = false, reading = false, notice = "", timer = null, active = false, generation = 0, revision = 0;
    const words = {
      unconfigured: "Remote connection: Not configured", connecting: "Remote connection: Connecting", connected: "Remote connection: Connected",
      retrying: "Remote connection: Reconnecting", restoring: "Remote connection: Restoring", "offline-detached": "Remote connection: Offline, local connections restored",
      paused: "Remote connection: Paused", "restore-failed": "Remote connection: Restoration incomplete", unknown: "Remote connection: Status unknown",
    };
    const say = key => { const value = t(key); return value.startsWith("Remote connection: ") ? value.slice(19) : value; };
    // A failed connect has to say what went wrong: the runner keeps the
    // client's precise reason, so name it instead of "an operation failed".
    const failures = {
      RESTORE_FAILED: "Remote connection: Restoration incomplete",
      model_refresh_failed: "Remote connection: Model refresh failed",
      registration_rejected: "Remote connection: Server refused this device",
      registration_request_invalid: "Remote connection: Server rejected the registration request",
      registration_response_invalid: "Remote connection: Server replied with something unexpected",
      tls_certificate_invalid: "Remote connection: Server certificate is not trusted",
      provider_binding_failed: "Remote connection: Server models could not be bound",
      connection_storage_unavailable: "Remote connection: Connection state could not be saved",
      OWNER_CONFLICT: "Remote connection: Another magpie is already running",
    };
    const unreachable = ["registration_timeout", "registration_unreachable", "dns_resolution_failed", "registration_failed"];
    const message = code => failures[code] || (unreachable.includes(code) ? "Remote connection: Server could not be reached" : "Remote connection: An operation failed. Try again.");
    function paint() {
      const q = id => page.querySelector("#" + id);
      q("remoteCurrent").textContent = status?.serverUrl || "";
      q("remoteStatus").textContent = say(words[status?.state] || words.unknown);
      q("remoteSync").textContent = status?.lastSyncAt && !isNaN(Date.parse(status.lastSyncAt)) ? say("Remote connection: Last sync") + ": " + new Date(status.lastSyncAt).toLocaleString(intlLang()) : say("Remote connection: No sync yet");
      const detail = notice || (status?.errorCode ? message(status.errorCode) : "");
      q("remoteNotice").textContent = detail && detail !== words[status?.state] ? say(detail) : "";
      q("remoteTarget").textContent = status?.state === "restore-failed" && ["workbuddy", "hermes"].includes(status.recoveryTarget) ? status.recoveryTarget : "";
      page.querySelectorAll("button").forEach(b => { b.disabled = busy || reading; });
      q("remoteManage").disabled = busy || reading || !status?.serverUrl;
      q("remoteRefresh").disabled = busy || reading || !status?.serverUrl;
      q("remoteDisconnect").disabled = busy || reading || !status?.serverUrl;
      page.setAttribute("aria-busy", String(busy || reading));
    }
    async function read(turn, version) {
      try {
        const result = await window.remoteConnection.request("status");
        if (!active || turn !== generation || version !== revision) return false;
        status = result;
        if (notice === "Remote connection: Status may be out of date") notice = "";
        // A later successful connection must clear the failure that came before
        // it, or the page keeps saying "could not be reached" while connected.
        else if (notice && !result?.errorCode) notice = "";
        return true;
      } catch {
        if (active && turn === generation && version === revision) notice = "Remote connection: Status may be out of date";
        return false;
      }
    }
    async function act(action) {
      if (busy || reading) return;
      if (action === "connect" && !page.querySelector("#remoteAddress").value.trim()) {
        notice = "Remote connection: Enter a server address first";
        paint();
        return;
      }
      busy = true; revision++; notice = ""; paint();
      try {
        if (action === "manage") {
          const answer = await window.remoteConnection.request("manage");
          const url = window.remoteConnection.managementURL(status?.serverUrl, answer?.url);
          if (!url) throw new Error();
          await api("open", { url });
        } else {
          status = await window.remoteConnection.request(action, action === "connect" ? page.querySelector("#remoteAddress").value.trim() : "");
          if (active && typeof status?.serverUrl === "string") page.querySelector("#remoteAddress").value = status.serverUrl;
        }
      } catch (error) {
        notice = message(error.code);
        if (active) await read(generation, revision);
      } finally {
        revision++;
        busy = false;
        if (active) paint();
      }
    }
    ctx.slots.add("page", {
      key: "remote-connection", title: "Server",
      html: `<div class="row-head"><span class="label" data-t>Remote connection: Server</span></div>
<div class="list"><div class="row pref remote-address-row"><div class="who"><label class="name" for="remoteAddress" data-t>Remote connection: Server address</label><div class="sub" data-t>Remote connection: Enter the server address once</div></div><input id="remoteAddress" type="url" autocomplete="url" spellcheck="false"></div>
<div class="row pref"><div class="who"><div class="sub" id="remoteCurrent"></div><div class="name" id="remoteStatus" role="status"></div><div class="sub" id="remoteSync"></div><div class="sub" id="remoteTarget"></div><div class="sub" id="remoteNotice" role="alert"></div></div></div></div>
<div class="after-list"><button class="text primary" id="remoteConnect" data-t>Remote connection: Connect</button> <button class="text" id="remoteRefresh" data-t>Remote connection: Refresh models</button> <button class="text" id="remoteManage" data-t>Remote connection: Open server management</button> <button class="text" id="remoteDisconnect" data-t>Remote connection: Pause</button></div>
<p class="usage-note" data-t>Remote connection: Manage local agents on the Agents page. Reconnecting the server does not reconnect them.</p><p class="usage-note" data-t>Remote connection: Changes in server management affect all clients.</p>`,
      mount(main) {
        page = main;
        for (const [id, action] of [["remoteConnect", "connect"], ["remoteRefresh", "refresh"], ["remoteManage", "manage"], ["remoteDisconnect", "disconnect"]]) page.querySelector("#" + id).onclick = () => act(action);
      },
      async open() {
        active = true;
        const turn = ++generation, version = revision;
        clearInterval(timer); timer = null;
        reading = true; paint();
        const ok = await read(turn, version);
        if (!active || turn !== generation) return;
        if (ok && !busy && typeof status?.serverUrl === "string") page.querySelector("#remoteAddress").value = status.serverUrl;
        reading = false; paint();
        timer = setInterval(async () => {
          if (busy || reading) return;
          const version = revision;
          reading = true; paint();
          await read(turn, version);
          if (!active || turn !== generation) return;
          reading = false; paint();
        }, 10000);
      },
      leave() { active = false; generation++; reading = false; clearInterval(timer); timer = null; },
    });
  });
}
