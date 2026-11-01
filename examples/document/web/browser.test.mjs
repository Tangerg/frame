import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import {
  mkdtemp,
  mkdir,
  readFile,
  writeFile,
  readdir,
  chmod,
  rm,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { chromium } from "playwright";
const directory = fileURLToPath(new URL(".", import.meta.url));

test(
  "real browser preview, original proofs and Host crash recovery",
  { timeout: 60000 },
  async (t) => {
    const binary = process.env.FRAME_DOCUMENT_BINARY;
    assert.ok(
      binary,
      "build the document command and set FRAME_DOCUMENT_BINARY",
    );
    const web = process.env.FRAME_DOCUMENT_WEB ?? join(directory, "dist");
    const executablePath = process.env.FRAME_CHROME_EXECUTABLE;
    const reportPath = process.env.FRAME_BROWSER_EVIDENCE;
    const state = await mkdtemp(join(tmpdir(), "frame-chrome-"));
    const documents = join(state, "documents");
    await mkdir(documents);
    await writeFile(
      join(documents, "hello.md"),
      "# Frame browser\nOriginal source revision.\n",
    );
    let server,
      exited,
      origin,
      diagnostics = "",
      browser;
    const errors = [];
    const observedPages = [];
    const failedRequests = [];
    const report = { startedAt: new Date().toISOString(), scenarios: {} };
    async function start(address = "127.0.0.1:0") {
      let current = "";
      server = spawn(
        binary,
        [
          "-command",
          "serve",
          "-state",
          state,
          "-documents",
          documents,
          "-listen",
          address,
          "-web",
          web,
        ],
        { stdio: ["ignore", "ignore", "pipe"] },
      );
      exited = once(server, "exit");
      server.stderr.setEncoding("utf8");
      server.stderr.on("data", (chunk) => {
        current += chunk;
        diagnostics += chunk;
      });
      const deadline = Date.now() + 15000;
      while (!/listening at http:\/\/[^;]+;/.test(current)) {
        if (server.exitCode !== null || Date.now() > deadline)
          throw new Error("document host failed to listen: " + current);
        await new Promise((resolve) => setTimeout(resolve, 10));
      }
      origin = /listening at (http:\/\/[^;]+);/.exec(current)[1];
    }
    async function connect(page, navigate = true) {
      observedPages.push(page);
      page.on("pageerror", (error) => errors.push(error.message));
      page.on("requestfailed", (request) =>
        failedRequests.push({
          url: request.url(),
          error: request.failure()?.errorText,
        }),
      );
      if (navigate) await page.goto(origin, { waitUntil: "domcontentloaded" });
      await page
        .locator("#token")
        .fill(
          (await readFile(join(state, "application-token"), "utf8")).trim(),
        );
      await page.locator("#connect").click();
      await page.waitForFunction(
        () =>
          document
            .querySelector("#connection-state")
            .textContent.startsWith("Realm ") ||
          document.querySelector("#error").textContent.length > 0,
        {},
        { timeout: 15000 },
      );
      assert.equal(await page.locator("#error").textContent(), "");
    }
    async function preview(page, id) {
      await page.locator("#task").fill(id);
      await page.locator("#convert").click();
      await page.waitForFunction(
        () => !!document.querySelector("iframe.document-preview"),
        {},
        { timeout: 15000 },
      );
      await page.waitForFunction(
        () => !document.querySelector("#convert").disabled,
      );
      assert.match(
        await page.locator("#status").textContent(),
        new RegExp(id + ": completed"),
      );
      assert.equal(
        await page.locator("iframe.document-preview").getAttribute("sandbox"),
        "",
      );
      assert.equal(await page.locator("#error").textContent(), "");
    }
    async function task(id) {
      const token = (
        await readFile(join(state, "application-token"), "utf8")
      ).trim();
      const response = await fetch(
        origin + "/api/tasks/" + encodeURIComponent(id),
        { headers: { Authorization: "Bearer " + token } },
      );
      assert.equal(response.status, 200);
      return response.json();
    }
    async function released(page) {
      await page.waitForFunction(
        () =>
          document.querySelector("#connection-state").textContent ===
          "Original Realm released and acknowledged",
        {},
        { timeout: 15000 },
      );
      assert.equal(await page.locator("iframe.document-preview").count(), 0);
      assert.equal(await page.locator("#error").textContent(), "");
    }
    async function writable(path) {
      await chmod(path, 0o700);
      for (const entry of await readdir(path, { withFileTypes: true }))
        if (entry.isDirectory()) await writable(join(path, entry.name));
    }
    try {
      await start();
      browser = await chromium.launch({
        executablePath,
        headless: true,
        args: ["--no-sandbox", "--disable-dev-shm-usage", "--no-proxy-server"],
      });
      report.browser = await browser.version();
      const context = await browser.newContext();
      const page = await context.newPage();
      await connect(page);
      await preview(page, "browser-original");
      assert.match(
        await page.locator("iframe.document-preview").getAttribute("srcdoc"),
        /<h1>Frame browser<\/h1>/,
      );
      const original = await task("browser-original");
      await writeFile(
        join(documents, "hello.md"),
        "# Changed source\nThe completed result must not change.\n",
      );
      await page.locator("#run").click();
      await page.waitForFunction(
        () => !document.querySelector("#run").disabled,
      );
      assert.deepEqual(await task("browser-original"), original);
      report.scenarios.originalTaskAndRealNativePreview = "passed";
      await page.evaluate(() => {
        globalThis.removedPreviewCount = 0;
        const observer = new MutationObserver((records) => {
          for (const record of records)
            for (const node of record.removedNodes)
              if (
                node instanceof HTMLIFrameElement &&
                node.classList.contains("document-preview")
              )
                globalThis.removedPreviewCount++;
        });
        observer.observe(document.querySelector("#preview"), {
          childList: true,
          subtree: true,
        });
        globalThis.previewObserver = observer;
      });
      let dropped = false,
        droppedProof,
        repeated = false;
      let observedDrop;
      const drop = new Promise((resolve) => (observedDrop = resolve));
      await page.route("**/frame/control", async (route) => {
        const body = route.request().postDataJSON();
        if (body.method === "prove") {
          if (!dropped) {
            const response = await route.fetch();
            assert.equal(response.status(), 200);
            const acknowledgement = await response.json();
            assert.equal(acknowledgement.recorded, true);
            dropped = true;
            droppedProof = body;
            await route.abort("failed");
            observedDrop();
            return;
          }
          if (body.input.proofId === droppedProof.input.proofId) {
            assert.deepEqual(body, droppedProof);
            repeated = true;
          }
        }
        await route.continue();
      });
      await page.locator("#close").click();
      let dropTimer;
      try {
        await Promise.race([
          drop,
          new Promise((_, reject) => {
            dropTimer = setTimeout(
              () => reject(new Error("proof loss injection was not reached")),
              15000,
            );
          }),
        ]);
      } finally {
        clearTimeout(dropTimer);
      }
      await page.waitForFunction(
        () => !document.querySelector("#close").disabled,
      );
      assert.notEqual(
        await page.locator("#connection-state").textContent(),
        "Original Realm released and acknowledged",
      );
      assert.equal(await page.locator("iframe.document-preview").count(), 0);
      await page.locator("#close").click();
      await released(page);
      assert.equal(
        repeated,
        true,
        "the original durable proof must be retransmitted unchanged",
      );
      assert.equal(
        await page.evaluate(() => globalThis.removedPreviewCount),
        1,
        "ack loss must not rerun DOM cleanup",
      );
      report.scenarios.committedProofAckLossAndOriginalRetry = "passed";
      await page.unroute("**/frame/control");
      await page.close();
      const survivor = await context.newPage();
      await connect(survivor);
      await preview(survivor, "browser-crash");
      const retained = await task("browser-crash");
      const address = new URL(origin).host;
      server.kill("SIGKILL");
      const [, signal] = await exited;
      assert.equal(signal, "SIGKILL");
      assert.equal(
        await survivor.locator("iframe.document-preview").count(),
        1,
      );
      await start(address);
      assert.deepEqual(await task("browser-crash"), retained);
      await survivor.locator("#reconnect").click();
      await survivor.waitForFunction(
        () =>
          document.querySelector("#connection-state").textContent ===
          "Original Realm: cleanupOnly",
        {},
        { timeout: 15000 },
      );
      await survivor.locator("#close").click();
      await released(survivor);
      report.scenarios.hostSigkillOriginalRealmCleanupOnlyAndDurableRelease =
        "passed";
      assert.equal(await survivor.locator("#connect").isDisabled(), true);
      await survivor.evaluate(() => {
        globalThis.originalRealmMarker = true;
      });
      await Promise.all([
        survivor.waitForEvent("load"),
        survivor.locator("#reload").click(),
      ]);
      assert.equal(
        await survivor.evaluate(() => "originalRealmMarker" in globalThis),
        false,
      );
      const fresh = survivor;
      await connect(fresh, false);
      await fresh.locator("#task").fill("browser-crash");
      await fresh.locator("#run").click();
      await fresh.waitForFunction(
        () => !document.querySelector("#run").disabled,
      );
      assert.deepEqual(await task("browser-crash"), retained);
      assert.equal(await fresh.locator("iframe.document-preview").count(), 1);
      await fresh.locator("#close").click();
      await released(fresh);
      report.scenarios.freshApprovalAfterOriginalExit = "passed";
      await fresh.close();
      await context.close();
      await browser.close();
      browser = undefined;
      server.kill("SIGINT");
      const [code] = await exited;
      assert.equal(code, 0, diagnostics);
      assert.deepEqual(errors, []);
      report.completedAt = new Date().toISOString();
      report.status = "passed";
      if (reportPath)
        await writeFile(reportPath, JSON.stringify(report, null, 2) + "\n");
      t.diagnostic(JSON.stringify(report));
    } catch (error) {
      report.status = "failed";
      report.error = error.stack;
      report.pageErrors = errors;
      report.failedRequests = failedRequests;
      report.pages = await Promise.all(
        observedPages
          .filter((page) => !page.isClosed())
          .map((page) =>
            page
              .evaluate(() => ({
                state: document.querySelector("#connection-state")?.textContent,
                error: document.querySelector("#error")?.textContent,
                status: document.querySelector("#status")?.textContent,
              }))
              .catch((error) => ({ error: String(error) })),
          ),
      );
      if (reportPath)
        await writeFile(reportPath, JSON.stringify(report, null, 2) + "\n");
      t.diagnostic(JSON.stringify(report));
      t.diagnostic(diagnostics);
      throw error;
    } finally {
      if (browser) await browser.close();
      if (server && server.exitCode === null && server.signalCode === null) {
        server.kill("SIGKILL");
        await exited;
      }
      await writable(state);
      await rm(state, { recursive: true, force: true });
    }
  },
);
