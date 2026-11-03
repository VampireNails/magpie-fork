const assert = require("node:assert/strict");
const { test } = require("node:test");
const fs = require("node:fs");
const { chromium, webkit } = require("playwright");
const { fixture } = require("./fixtures/caller-keys.cjs");

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(`${engine}: devices paginate and removing one preserves gateway keys`, async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const page = await (await browser.newContext({ viewport: { width: 390, height: 844 }, reducedMotion: "reduce" })).newPage();
    const errors = [], deleted = [];
    page.on("pageerror", e => errors.push(e.message));
    await page.route("**/*", fixture("en", "light", [], { lan: true, web: true }));
    if (process.env.OLD_DEVICE_KEYS) await page.route("**/plugins/gateway/keys.js*", route => route.fulfill({ contentType: "text/javascript", body: fs.readFileSync(process.env.OLD_DEVICE_KEYS) }));
    const devices = [
      { id: "aaaaaaaaaaaaaaaaaaaaaaaa", displayName: "Laptop node", nodeId: "123", lastSeenAt: "2026-11-02T12:00:00Z" },
      { id: "bbbbbbbbbbbbbbbbbbbbbbbb", displayName: "Server node", nodeId: "456", lastSeenAt: "2026-11-02T12:01:00Z" },
    ];
    await page.route("**/api/remote-devices**", async route => {
      const req = route.request(), url = new URL(req.url());
      if (req.method() === "DELETE") {
        const id = url.pathname.split("/").pop(); deleted.push(id);
        devices.splice(devices.findIndex(d => d.id === id), 1);
        return route.fulfill({ status: 204 });
      }
      const next = !url.searchParams.has("cursor") && devices.length > 1;
      return route.fulfill({ json: { items: next ? devices.slice(0, 1) : url.searchParams.has("cursor") ? devices.slice(1) : devices, nextCursor: next ? devices[0].id : null } });
    });
    await page.goto("http://magpie.test/?view=gateway");
    await page.locator('[data-device="bbbbbbbbbbbbbbbbbbbbbbbb"]').waitFor();
    assert.equal(await page.locator("#gatewayKeys .acc[data-key]").count(), 3);
    assert.match(await page.locator(".remote-devices").innerText(), /Last registration is not an online status/);
    await page.locator('[data-device="aaaaaaaaaaaaaaaaaaaaaaaa"]').getByRole("button", { name: "Remove device" }).click();
    await page.getByRole("alertdialog").getByRole("button", { name: "Remove", exact: true }).click();
    await page.locator('[data-device="aaaaaaaaaaaaaaaaaaaaaaaa"]').waitFor({ state: "detached" });
    assert.deepEqual(deleted, ["aaaaaaaaaaaaaaaaaaaaaaaa"]);
    assert.equal(await page.locator('[data-device="bbbbbbbbbbbbbbbbbbbbbbbb"]').count(), 1);
    assert.equal(await page.locator("#gatewayKeys .acc[data-key]").count(), 3);
    assert.deepEqual(errors, []);
  });

  test(`${engine}: devices remain visible when local network sharing is off`, async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const page = await (await browser.newContext({ viewport: { width: 390, height: 844 } })).newPage();
    await page.route("**/*", fixture("en", "light", [], { lan: false, web: true }));
    await page.route("**/api/remote-devices**", route => route.fulfill({ json: { items: [{ id: "aaaaaaaaaaaaaaaaaaaaaaaa", displayName: "Remote node", nodeId: "123", lastSeenAt: "2026-11-02T12:00:00Z" }], nextCursor: null } }));
    await page.goto("http://magpie.test/?view=gateway");
    await page.locator('[data-device="aaaaaaaaaaaaaaaaaaaaaaaa"]').waitFor({ state: "visible" });
    assert.equal(await page.locator("#gatewayKeysBlock").isVisible(), false);
  });
}
