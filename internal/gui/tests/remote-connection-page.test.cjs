const { chromium, webkit } = require("playwright");
const fs = require("node:fs/promises");
const path = require("node:path");
const assert = require("node:assert/strict");
const { test } = require("node:test");
const assets = path.resolve(__dirname, "../assets");
for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(engine + ": remote connection is local and does not connect agents", async t => {
    const browser = await (engine === "webkit" ? webkit : chromium).launch();
    t.after(() => browser.close());
    for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) for (const client of [true, false]) {
      const page = await browser.newPage({ viewport: { width: 420, height: 700 }, reducedMotion: "reduce" });
      page.setDefaultNavigationTimeout(60000);
      const calls = [], errors = []; let failed = false;
      page.on("pageerror", e => errors.push(e.stack || e.message));
      await page.route("**/*", async route => {
        const req = route.request(), u = new URL(req.url());
        const json = data => route.fulfill({ json: data });
        if (u.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs={lang:${JSON.stringify(lang)},theme:"light",web:${!client},remoteClient:${client}}` });
        if (u.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
        if (u.pathname.startsWith("/api/remote-connection")) {
          calls.push([u.pathname,req.method(),req.postData()]);
          if (req.method() === "POST" && req.postData()?.includes("other.example")) { failed = true; return route.fulfill({status:503,json:{error:{code:"RESTORE_FAILED",message:"secret should never appear"}}}); }
          return json({ serverUrl:"https://node.example:9443",state:failed ? "restore-failed" : "connected",owner:true,connected:!failed,lastSyncAt:null,errorCode:failed ? "RESTORE_FAILED" : "",recoveryTarget:failed ? "workbuddy" : "" });
        }
        if (u.pathname.startsWith("/api/agents/") || u.pathname === "/api/set") calls.push([u.pathname,req.method()]);
        if (u.pathname === "/api/state") return json({ agents:[], profiles:[], settings:{lang,theme:"light"} });
        if (u.pathname === "/api/settings") return json({lang,theme:"light"});
        if (u.pathname === "/api/plugins") return json({plugins:[]});
        if (u.pathname === "/api/groups") return json({groups:[],models:[]});
        if (u.pathname.startsWith("/api/")) return json({});
        const file = path.join(assets,u.pathname === "/" ? "index.html" : u.pathname);
        const type = {".html":"text/html",".js":"text/javascript",".css":"text/css",".svg":"image/svg+xml"}[path.extname(file)];
        try { return await route.fulfill({body:await fs.readFile(file),contentType:type}); } catch { return route.fulfill({status:404,body:""}); }
      });
      await page.goto("http://localhost:3430/");
      const nav = page.locator('#nav button[data-view="remote-connection"]');
      if (!client) { assert.equal(await nav.count(),0); assert.equal(calls.filter(c => c[0].startsWith("/api/remote-connection")).length,0); }
      else {
        await nav.click();
        await page.locator('#remoteConnect').click();
        await page.waitForFunction(() => !document.querySelector('#remoteConnect').disabled);
        assert(calls.some(c => c[0] === "/api/remote-connection" && c[1] === "POST"));
        await page.locator("#remoteAddress").fill("https://other.example:9443");
        await page.locator("#remoteConnect").click();
        await page.waitForFunction(() => document.querySelector('#remoteTarget').textContent.includes('workbuddy'));
        assert((await page.locator('#remoteCurrent').textContent()).includes('node.example'));
        assert(!(await page.locator('#view-remote-connection').textContent()).includes('secret should never appear'));
        assert(!calls.some(c => c[1] !== 'GET' && (c[0].startsWith('/api/agents/') || c[0] === '/api/set')));
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
        if (process.env.ARTIFACT_DIR && lang === "zh") { await fs.mkdir(process.env.ARTIFACT_DIR,{recursive:true}); await page.screenshot({path:path.join(process.env.ARTIFACT_DIR,engine+'-connection.png')}); }
      }
      assert.deepEqual(errors,[]);
      await page.close();
    }
  });
}
