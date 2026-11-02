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
      if (!response.ok) throw new Error();
      return await response.json();
    } catch {
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
