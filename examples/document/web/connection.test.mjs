import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { setImmediate } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { Window } from "happy-dom";
import { build } from "vite";

const root = fileURLToPath(new URL(".", import.meta.url));
const actionTimeoutMilliseconds = 5_000;

async function withApplication(t, intercept, inspect) {
  const bundle = await build({
    root,
    logLevel: "silent",
    build: { write: false },
  });
  const entry = bundle.output.find(
    (output) => output.type === "chunk" && output.isEntry,
  );
  assert.ok(entry);
  const window = new Window({
    url: "http://127.0.0.1:8088",
    settings: {
      disableCSSFileLoading: true,
      disableJavaScriptFileLoading: true,
      disableIframePageLoading: true,
    },
  });
  window.document.write(
    await readFile(new URL("index.html", import.meta.url), "utf8"),
  );
  const originals = {
    window: globalThis.window,
    document: globalThis.document,
    HTMLElement: globalThis.HTMLElement,
    MutationObserver: globalThis.MutationObserver,
    fetch: globalThis.fetch,
  };
  Object.assign(globalThis, {
    window,
    document: window.document,
    HTMLElement: window.HTMLElement,
    MutationObserver: window.MutationObserver,
  });
  const authority = { id: "host", epoch: "epoch" };
  const realm = { id: "realm", epoch: "epoch" };
  const credentials = {
    authority,
    realm,
    token: "test-token",
    resumeToken: "test-resume",
    maxMessageBytes: 1_048_576,
  };
  const requests = [];
  const element = (id) => window.document.getElementById(id);
  async function click(id) {
    element(id).click();
    const deadline = Date.now() + actionTimeoutMilliseconds;
    while (element(id).disabled) {
      if (Date.now() > deadline)
        throw new Error(`action did not settle: ${id}`);
      await setImmediate();
    }
  }
  globalThis.fetch = async (input, init) => {
    const path = new URL(input, window.location.href).pathname;
    const body = init?.body ? JSON.parse(init.body) : undefined;
    requests.push({ path, body });
    return intercept({ path, body, signal: init?.signal }, async () => {
      switch (path) {
        case "/api/realm/open":
        case "/api/realm/resume":
          return Response.json(credentials);
        case "/api/contracts":
          return Response.json({
            previews: {
              name: "documents.preview",
              version: "1",
              digest: "a".repeat(64),
              kind: "point",
              mode: "native",
            },
          });
        case "/api/preview/activate":
          throw new TypeError("activation transport unavailable");
        case "/api/realm/seal":
        case "/api/recover":
        case "/api/queue":
          return Response.json(null);
        case "/api/run":
          return Response.json({
            id: "original-task",
            state: "completed",
            binding: { units: [] },
            result: {
              converted: { html: "<p>kept</p>", title: "Kept" },
              diagnostics: [],
            },
          });
        case "/frame/control":
          if (body.method === "poll")
            return Response.json({ authority, realm, plans: [], fences: [] });
          if (body.method === "reconnect")
            return Response.json({
              authority,
              realm,
              mode: "revalidated",
              executions: [],
            });
          throw new Error(`unexpected control method: ${body.method}`);
        default:
          throw new Error(`unexpected request: ${path}`);
      }
    });
  };
  let loaded = false;
  try {
    await import(
      "data:text/javascript;base64," +
        Buffer.from(entry.code).toString("base64") +
        "#" +
        encodeURIComponent(t.name)
    );
    loaded = true;
    await inspect({ click, element, requests, realm });
  } finally {
    try {
      if (loaded) await click("close");
    } finally {
      await window.happyDOM.abort();
      Object.assign(globalThis, originals);
    }
  }
}

async function until(predicate, message) {
  const deadline = Date.now() + actionTimeoutMilliseconds;
  while (!predicate()) {
    if (Date.now() > deadline) throw new Error(message);
    await setImmediate();
  }
}

test("connection recovery retries activation in the original Realm and exposes an unavailable preview", async (t) => {
  await withApplication(
    t,
    (_request, next) => next(),
    async ({ click, element, requests, realm }) => {
      await click("connect");
      assert.match(element("error").textContent, /activation transport/);
      await click("connect");
      assert.match(element("error").textContent, /activation transport/);
      await click("reconnect");
      assert.match(element("error").textContent, /activation transport/);
      assert.equal(
        requests.filter(({ path }) => path === "/api/realm/open").length,
        1,
      );
      const activations = requests.filter(
        ({ path }) => path === "/api/preview/activate",
      );
      assert.equal(activations.length, 3);
      for (const { body } of activations) assert.deepEqual(body.realm, realm);
      element("task").value = "original-task";
      await click("convert");
      assert.match(element("status").textContent, /original-task: completed/);
      assert.match(
        element("error").textContent,
        /activation has not completed/,
      );
      assert.equal(element("preview").querySelector("iframe"), null);
      await click("close");
      assert.match(element("connection-state").textContent, /released/);
      assert.equal(element("reload").disabled, false);
    },
  );
});

test("release cancels pending activation before joining it and waits for actual completion", async (t) => {
  const activation = Promise.withResolvers();
  let activationSignal;
  await withApplication(
    t,
    async (request, next) => {
      if (request.path === "/api/preview/activate") {
        activationSignal = request.signal;
        await activation.promise;
        request.signal.throwIfAborted();
      }
      return next();
    },
    async ({ click, element, requests }) => {
      element("connect").click();
      await until(
        () => activationSignal !== undefined,
        "activation was not dispatched",
      );
      element("close").click();
      try {
        await until(
          () => requests.some(({ path }) => path === "/api/realm/seal"),
          "release waited behind activation",
        );
        assert.equal(activationSignal.aborted, true);
        assert.equal(
          element("close").disabled,
          true,
          "incomplete work cannot claim released",
        );
        assert.equal(element("reload").disabled, true);
        element("reconnect").click();
        assert.equal(
          requests.filter(({ path }) => path === "/api/realm/resume").length,
          0,
        );
      } finally {
        activation.resolve();
      }
      await until(
        () => !element("close").disabled,
        "release did not join the original activation",
      );
      assert.equal(element("error").textContent, "");
      assert.match(element("connection-state").textContent, /released/);
      assert.equal(
        requests.filter(({ path }) => path === "/api/realm/open").length,
        1,
      );
      assert.equal(
        requests.filter(({ path }) => path === "/api/preview/activate").length,
        1,
      );
      assert.equal(element("connect").disabled, true);
      await click("close");
    },
  );
});

test("release resolves an uncertain opening with its original identity before sealing it", async (t) => {
  const opening = Promise.withResolvers();
  await withApplication(
    t,
    async (request, next) => {
      if (request.path === "/api/realm/open") await opening.promise;
      return next();
    },
    async ({ element, requests }) => {
      element("connect").click();
      await until(
        () => requests.some(({ path }) => path === "/api/realm/open"),
        "opening was not dispatched",
      );
      element("close").click();
      try {
        await until(
          () =>
            requests.filter(({ path }) => path === "/api/realm/open").length ===
            2,
          "cleanup did not resolve the original opening",
        );
        const opens = requests.filter(({ path }) => path === "/api/realm/open");
        assert.deepEqual(opens[1].body, opens[0].body);
        assert.equal(element("close").disabled, true);
        assert.equal(element("reload").disabled, true);
        assert.equal(
          requests.filter(({ path }) => path === "/api/realm/seal").length,
          0,
        );
      } finally {
        opening.resolve();
      }
      await until(
        () => !element("close").disabled,
        "original Realm did not settle",
      );
      assert.equal(element("error").textContent, "");
      assert.equal(
        requests.filter(({ path }) => path === "/api/realm/seal").length,
        1,
      );
      assert.equal(
        requests.filter(({ path }) => path === "/api/contracts").length,
        0,
      );
      assert.equal(
        requests.filter(({ path }) => path === "/frame/control").length,
        0,
      );
      assert.equal(
        requests.filter(({ path }) => path === "/api/preview/activate").length,
        0,
      );
      assert.equal(element("connect").disabled, true);
      assert.equal(element("reload").disabled, false);
    },
  );
});
