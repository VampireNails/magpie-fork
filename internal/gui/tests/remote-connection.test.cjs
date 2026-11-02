const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const path = require("node:path");
const file = path.join(__dirname, "../assets/remote-connection.js");
function load(fetch) {
  const context = { URL, fetch, window: {} };
  if (fs.existsSync(file)) vm.runInNewContext(fs.readFileSync(file, "utf8"), context);
  return context.window.remoteConnection || { managementURL: () => null, request: async () => null };
}
test("management link is clean and belongs to the saved server", () => {
  const api = load();
  const base = "https://node.example:9443";
  const good = base + "/_magpie/remote/v1/browser-login";
  assert.equal(api.managementURL(base, good), good);
  for (const bad of ["https://other.example/_magpie/remote/v1/browser-login", "http://node.example:9443/_magpie/remote/v1/browser-login", "https://u@node.example:9443/_magpie/remote/v1/browser-login", good + "?k=secret", good + "#fragment", good + "?", good + "#", "https://@node.example:9443/_magpie/remote/v1/browser-login"]) assert.equal(api.managementURL(base, bad), null);
});
test("requests stay local and do not expose backend error bodies", async () => {
  const calls = [];
  const api = load(async (url, options) => { calls.push([url, options]); return { ok: false, json: async () => ({ error: { message: "secret" } }) }; });
  await assert.rejects(api.request("connect", "https://node.example"), e => e.message === "remote connection request failed");
  assert.equal(calls[0][0], "/api/remote-connection");
  assert.deepEqual(JSON.parse(calls[0][1].body), { serverUrl: "https://node.example" });
  await assert.rejects(api.request("invalid"));
  assert.equal(calls.length, 1);
});

test("management links are checked in Chromium and WebKit", async (t) => {
  const { chromium, webkit } = require("playwright");
  for (const [name, engine] of [["chromium", chromium], ["webkit", webkit]]) {
    await t.test(name, async () => {
      const browser = await engine.launch();
      try {
        const page = await browser.newPage();
        await page.addScriptTag({ path: file });
        const result = await page.evaluate(() => {
          const base = "https://node.example:9443";
          const good = base + "/_magpie/remote/v1/browser-login";
          return [window.remoteConnection.managementURL(base, good), window.remoteConnection.managementURL(base, good + "?k=secret")];
        });
        assert.deepEqual(result, ["https://node.example:9443/_magpie/remote/v1/browser-login", null]);
        console.log(name + " " + browser.version());
      } finally { await browser.close(); }
    });
  }
});
