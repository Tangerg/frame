import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import {
  chmod,
  mkdtemp,
  mkdir,
  readFile,
  readdir,
  rm,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import test from "node:test";
import { Window } from "happy-dom";
import { build } from "vite";

const directory = fileURLToPath(new URL(".", import.meta.url));
const binary = process.env.FRAME_DOCUMENT_BINARY;

// This is a real HTTP/Go-process integration using a DOM implementation. It
// does not claim browser-engine coverage or make browser unload an exit proof.
test("document CLI, Host control, actual JS preview and durable exit", async () => {
  assert.ok(binary, "build the document command and set FRAME_DOCUMENT_BINARY");
  const state = await mkdtemp(join(tmpdir(), "frame-document-workflow-"));
  const documents = join(state, "documents");
  await mkdir(documents);
  await writeFile(
    join(documents, "hello.md"),
    "# Frame document\nA retained conversion.\n",
  );
  const server = spawn(
    binary,
    [
      "-command",
      "serve",
      "-state",
      state,
      "-documents",
      documents,
      "-listen",
      "127.0.0.1:0",
      "-web",
      join(directory, "dist"),
    ],
    { stdio: ["ignore", "ignore", "pipe"] },
  );
  const exited = once(server, "exit");
  let diagnostics = "";
  server.stderr.setEncoding("utf8");
  server.stderr.on("data", (chunk) => {
    diagnostics += chunk;
  });
  let window;
  let completed = false;
  const originalFetch = globalThis.fetch;
  try {
    await until(
      () => /listening at http:\/\/[^;]+;/.test(diagnostics),
      () => diagnostics,
    );
    const origin = /listening at (http:\/\/[^;]+);/.exec(diagnostics)[1];
    window = new Window({
      url: origin,
      settings: {
        disableCSSFileLoading: true,
        disableJavaScriptFileLoading: true,
        disableIframePageLoading: true,
      },
    });
    window.document.write(
      await readFile(join(directory, "dist/index.html"), "utf8"),
    );
    Object.assign(globalThis, {
      window,
      document: window.document,
      HTMLElement: window.HTMLElement,
      MutationObserver: window.MutationObserver,
    });
    let rejectTheme = true;
    let activationRequests = 0;
    let realmOpens = 0;
    let realmSeals = 0;
    let suspendTheme = false;
    let themeSignal;
    const previewExecutions = new Set();
    let releasedPreviewUses = 0;
    let loseAcquire = false;
    let lostRequest, lostGrant;
    let replayed = false,
      recovered = false;
    let prematureSnapshot = false;
    globalThis.fetch = async (input, init) => {
      const url = new URL(input instanceof Request ? input.url : input, origin);
      if (url.pathname === "/api/realm/open") realmOpens++;
      if (url.pathname === "/api/realm/seal") realmSeals++;
      if (url.pathname === "/api/preview/activate") {
        activationRequests++;
        if (activationRequests === 1)
          throw new Error("activation transport failed before dispatch");
      }
      if (url.pathname === "/api/theme" && rejectTheme) {
        rejectTheme = false;
        return Response.json({ error: "theme failure" }, { status: 500 });
      }
      if (url.pathname === "/api/theme" && suspendTheme) {
        themeSignal = init.signal;
        await new Promise((_resolve, reject) => {
          const cancel = () => reject(themeSignal.reason);
          if (themeSignal.aborted) cancel();
          else themeSignal.addEventListener("abort", cancel, { once: true });
        });
      }
      const reply = await originalFetch(url, init);
      if (
        url.pathname === "/api/preview/activate" &&
        activationRequests === 2
      ) {
        assert.equal(reply.ok, true);
        throw new Error("activation acknowledgement lost after host commit");
      }
      if (url.pathname === "/frame/control" && reply.ok) {
        const message = JSON.parse(init.body);
        if (message.method === "ready")
          previewExecutions.add(message.input.execution.id);
        if (message.method === "acquire" && loseAcquire) {
          loseAcquire = false;
          lostRequest = message.input;
          lostGrant = await reply.json();
          throw new Error("acquire acknowledgement lost after host commit");
        }
        if (lostRequest && !recovered && message.method === "snapshot")
          prematureSnapshot = true;
        if (message.method === "acquire" && lostRequest && !replayed) {
          assert.deepEqual(message.input, lostRequest);
          assert.equal((await reply.clone().json()).id, lostGrant.id);
          replayed = true;
        }
        if (message.method === "prove" && message.input.scopeKind === "use") {
          releasedPreviewUses++;
          if (lostGrant && message.input.useId === lostGrant.id)
            recovered = true;
        }
      }
      return reply;
    };
    await import(entryModule(window.document));
    const element = (id) => window.document.getElementById(id);
    element("token").value = (
      await readFile(join(state, "application-token"), "utf8")
    ).trim();
    element("connect").click();
    await until(
      () => !element("connect").disabled,
      () => element("error").textContent + diagnostics,
    );
    assert.match(element("error").textContent, /before dispatch/);
    element("connect").click();
    await until(
      () => !element("connect").disabled,
      () => element("error").textContent + diagnostics,
    );
    assert.match(element("error").textContent, /after host commit/);
    element("reconnect").click();
    await until(
      () => !element("reconnect").disabled,
      () => element("error").textContent + diagnostics,
    );
    assert.equal(element("error").textContent, "");
    assert.equal(
      element("connection-state").textContent,
      "Original Realm: connected",
    );
    assert.equal(activationRequests, 3);
    assert.equal(realmOpens, 1);
    assert.equal(previewExecutions.size, 1);
    element("task").value = "workflow-original";
    element("convert").click();
    await until(
      () =>
        !element("convert").disabled &&
        element("error").textContent === "theme failure",
      () => element("error").textContent + diagnostics,
    );
    assert.equal(
      releasedPreviewUses,
      1,
      "failed mount must release its acquired Use",
    );
    assert.equal(
      window.document.querySelector("iframe.document-preview"),
      null,
    );
    loseAcquire = true;
    element("run").click();
    await until(
      () => !element("run").disabled && !!lostRequest,
      () => diagnostics,
    );
    assert.match(element("error").textContent, /control transport/);
    assert.equal(releasedPreviewUses, 1);
    element("reconnect").click();
    await until(
      () => !element("reconnect").disabled && recovered,
      () => element("error").textContent + diagnostics,
    );
    assert.ok(
      replayed,
      "recovery must reuse the original acquire request and grant",
    );
    assert.equal(releasedPreviewUses, 2);
    assert.equal(
      prematureSnapshot,
      false,
      "uncertain grants must be settled before selecting again",
    );
    element("run").click();
    await until(
      () => window.document.querySelector("iframe.document-preview"),
      () => element("error").textContent + diagnostics,
    );
    const frame = window.document.querySelector("iframe.document-preview");
    assert.match(frame.srcdoc, /<h1>Frame document<\/h1>/);
    assert.equal(frame.getAttribute("sandbox"), "");
    assert.match(element("status").textContent, /workflow-original: completed/);
    const request = {
      headers: { Authorization: `Bearer ${element("token").value}` },
    };
    const original = await (
      await originalFetch(origin + "/api/tasks/workflow-original", request)
    ).json();
    await writeFile(join(documents, "hello.md"), "# Changed source\n");
    suspendTheme = true;
    element("run").click();
    await until(
      () => themeSignal !== undefined,
      () => element("error").textContent,
    );
    const repeated = await (
      await originalFetch(origin + "/api/tasks/workflow-original", request)
    ).json();
    assert.deepEqual(
      repeated,
      original,
      "the original task must be observed without conversion replay",
    );
    element("close").click();
    await until(
      () => realmSeals === 1 && themeSignal.aborted,
      () => "release is waiting behind the pending preview mount",
    );
    await until(
      () =>
        element("connection-state").textContent ===
        "Original Realm released and acknowledged",
      () => element("error").textContent + diagnostics,
    );
    assert.equal(
      window.document.querySelector("iframe.document-preview"),
      null,
    );
    assert.equal(element("error").textContent, "");
    assert.equal(element("connect").disabled, true);
    assert.equal(element("reload").disabled, false);
    assert.equal(element("run").disabled, false);
    assert.equal(realmOpens, 1);
    assert.equal(previewExecutions.size, 1);
    completed = true;
  } finally {
    globalThis.fetch = originalFetch;
    await window?.happyDOM.close();
    if (server.exitCode === null) server.kill(completed ? "SIGINT" : "SIGKILL");
    const [code] = await exited;
    await writable(state);
    await rm(state, { recursive: true, force: true });
    if (completed) assert.equal(code, 0, diagnostics);
  }
});

test("dynamic native publication exits through its original execution and resends a lost proof acknowledgement", async () => {
  assert.ok(binary, "build the document command and set FRAME_DOCUMENT_BINARY");
  const state = await mkdtemp(join(tmpdir(), "frame-document-publication-"));
  const documents = join(state, "documents");
  await mkdir(documents);
  const fixture = join(directory, "control-workflow-fixture.ts");
  await build({
    root: directory,
    configFile: false,
    logLevel: "silent",
    plugins: [
      {
        name: "original-browser-runtime-fixture",
        resolveId(id) {
          if (id === fixture) return id;
        },
        load(id) {
          if (id === fixture)
            return [
              'export { HTTPControl, RealmModules, Runtime, select } from "@frame/sdk/browser";',
              'export { Lifetime, Point } from "@frame/sdk";',
            ].join("\n");
        },
      },
    ],
    build: {
      outDir: join(state, "fixture"),
      lib: {
        entry: fixture,
        formats: ["es"],
        fileName: () => "runtime.mjs",
      },
    },
  });
  const { HTTPControl, RealmModules, Runtime, select, Lifetime, Point } =
    await import(pathToFileURL(join(state, "fixture/runtime.mjs")).href);
  const server = spawn(
    binary,
    [
      "-command",
      "serve",
      "-state",
      state,
      "-documents",
      documents,
      "-listen",
      "127.0.0.1:0",
    ],
    { stdio: ["ignore", "ignore", "pipe"] },
  );
  const exited = once(server, "exit");
  let diagnostics = "";
  server.stderr.setEncoding("utf8");
  server.stderr.on("data", (chunk) => {
    diagnostics += chunk;
  });
  let completed = false;
  const transport = new Lifetime("document control workflow");
  try {
    await until(
      () => /listening at http:\/\/[^;]+;/.test(diagnostics),
      () => diagnostics,
    );
    const origin = /listening at (http:\/\/[^;]+);/.exec(diagnostics)[1];
    const token = (
      await readFile(join(state, "application-token"), "utf8")
    ).trim();
    const api = async (path, input) => {
      const response = await fetch(origin + path, {
        method: input === undefined ? "GET" : "POST",
        headers: {
          Authorization: `Bearer ${token}`,
          "Content-Type": "application/json",
        },
        ...(input === undefined ? {} : { body: JSON.stringify(input) }),
      });
      const value = await response.json();
      assert.equal(response.ok, true, JSON.stringify(value));
      return value;
    };
    const credentials = await api("/api/realm/open", {
      operationId: crypto.randomUUID(),
      resumeToken: crypto.randomUUID() + crypto.randomUUID(),
    });
    const point = new Point((await api("/api/contracts")).previews);
    const proofs = [];
    let loseAcknowledgement = true;
    const control = new HTTPControl({
      endpoint: origin + "/frame/control",
      credential: credentials.token,
      maximumMessageBytes: credentials.maxMessageBytes,
      lifetime: transport,
      async fetch(input, init) {
        const request = JSON.parse(init.body);
        if (request.method === "prove") proofs.push(request.input);
        const response = await fetch(input, init);
        if (
          request.method === "prove" &&
          request.input.scopeKind === "execution" &&
          loseAcknowledgement
        ) {
          assert.equal(response.ok, true);
          assert.equal((await response.json()).recorded, true);
          loseAcknowledgement = false;
          throw new Error("original execution proof acknowledgement lost");
        }
        return response;
      },
    });
    const modules = new RealmModules(async (unit, signal) => {
      const response = await fetch(new URL(unit.module.path, origin), {
        signal,
      });
      assert.equal(response.ok, true);
      const bytes = new Uint8Array(await response.arrayBuffer());
      return {
        bytes,
        close() {},
      };
    });
    let cleanups = 0;
    const runtime = new Runtime({
      authority: credentials.authority,
      realm: credentials.realm,
      control,
      proofCapacity: 128,
      load(unit, owner, plan) {
        owner.cleanup(() => {
          cleanups++;
        });
        return modules.load(unit, owner, plan);
      },
      remote() {
        throw new Error("this consumer has no remote import contract");
      },
    });
    const activating = api("/api/preview/activate", credentials);
    let plan;
    const deadline = Date.now() + 15000;
    while (!plan) {
      plan = (await control.poll()).plans[0];
      if (!plan) {
        assert.ok(Date.now() < deadline, diagnostics);
        await new Promise((resolve) => setTimeout(resolve, 10));
      }
    }
    await runtime.prepare(plan);
    await activating;
    const native = { mount() {} };
    const publication = await runtime.publish(
      plan.execution,
      plan.units[0].instance.id,
      point,
      "dynamic-preview",
      { format: "html", priority: 99 },
      native,
    );
    assert.equal(publication.outcome.decision, "accepted");
    const snapshot = await control.snapshot({
      operationId: crypto.randomUUID(),
      points: [point.ref.name],
    });
    const choice = select(
      snapshot,
      point,
      "documents.dynamic-preview/1",
      (choices) =>
        choices.filter(
          (entry) => entry.candidate.source.export === "dynamic-preview",
        ),
      (metadata) => metadata,
    );
    assert.equal(choice.candidates.length, 1);
    const use = await runtime.acquire({
      operationId: crypto.randomUUID(),
      snapshotId: snapshot.id,
      execution: plan.execution,
      selections: [choice],
    });
    await use.with(publication.object, point, (value) => {
      assert.equal(
        value,
        native,
        "the approved Use must retain the original native object",
      );
    });
    await assert.rejects(runtime.close(), { code: "outcome_unknown" });
    assert.equal(cleanups, 1);
    assert.deepEqual(
      runtime.pendingProofs.map((proof) => proof.scopeKind),
      ["execution"],
    );
    const original = runtime.pendingProofs[0];
    await runtime.close();
    assert.equal(cleanups, 1, "proof retry must not repeat cleanup");
    assert.equal(runtime.pendingProofs.length, 0);
    assert.deepEqual(
      proofs.filter((proof) => proof.scopeKind === "execution"),
      [original, original],
    );
    assert.equal(
      proofs.some((proof) => proof.scopeKind === "publication"),
      false,
      "execution-owned cleanup must not manufacture a separately withdrawn publication exit",
    );
    await transport.close();
    completed = true;
  } finally {
    if (server.exitCode === null) server.kill(completed ? "SIGINT" : "SIGKILL");
    const [code] = await exited;
    await writable(state);
    await rm(state, { recursive: true, force: true });
    if (completed) assert.equal(code, 0, diagnostics);
  }
});

test("document actions retain their original task when the selection changes", async () => {
  const window = new Window({
    url: "http://127.0.0.1:8088",
    settings: {
      disableCSSFileLoading: true,
      disableJavaScriptFileLoading: true,
      disableIframePageLoading: true,
    },
  });
  const original = {
    window: globalThis.window,
    document: globalThis.document,
    HTMLElement: globalThis.HTMLElement,
    MutationObserver: globalThis.MutationObserver,
    fetch: globalThis.fetch,
  };
  const queue = Promise.withResolvers();
  const cancel = Promise.withResolvers();
  const calls = [];
  try {
    window.document.write(
      await readFile(join(directory, "dist/index.html"), "utf8"),
    );
    Object.assign(globalThis, {
      window,
      document: window.document,
      HTMLElement: window.HTMLElement,
      MutationObserver: window.MutationObserver,
      fetch: async (path, init) => {
        const id = init.body
          ? JSON.parse(init.body).id
          : decodeURIComponent(path.split("/").at(-1));
        calls.push({ path, id });
        if (path === "/api/queue") await queue.promise;
        if (path === "/api/cancel") await cancel.promise;
        return Response.json({
          id,
          state: "completed",
          binding: { units: [] },
        });
      },
    });
    const main = entryModule(window.document);
    await import(main + "?original-task-actions");
    const element = (id) => window.document.getElementById(id);
    element("token").value = "action-test-credential";
    element("task").value = "convert-original";
    element("convert").click();
    await until(
      () => calls.length === 1,
      () => JSON.stringify(calls),
    );
    element("task").value = "convert-new-selection";
    element("status").textContent = "new selection";
    queue.resolve();
    await until(
      () => !element("convert").disabled,
      () => element("error").textContent,
    );
    assert.deepEqual(calls, [
      { path: "/api/queue", id: "convert-original" },
      { path: "/api/run", id: "convert-original" },
    ]);
    assert.equal(element("status").textContent, "new selection");

    element("task").value = "cancel-original";
    element("cancel").click();
    await until(
      () => calls.length === 3,
      () => JSON.stringify(calls),
    );
    element("task").value = "cancel-new-selection";
    cancel.resolve();
    await until(
      () => !element("cancel").disabled,
      () => element("error").textContent,
    );
    assert.deepEqual(calls.slice(2), [
      { path: "/api/cancel", id: "cancel-original" },
      { path: "/api/tasks/cancel-original", id: "cancel-original" },
    ]);
    assert.equal(element("status").textContent, "new selection");
    assert.equal(element("error").textContent, "");
  } finally {
    queue.resolve();
    cancel.resolve();
    Object.assign(globalThis, original);
    await window.happyDOM.close();
  }
});

async function until(predicate, diagnostic) {
  const deadline = Date.now() + 15000;
  while (!predicate()) {
    if (Date.now() >= deadline)
      throw new Error("workflow did not complete: " + diagnostic());
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
}
async function writable(path) {
  await chmod(path, 0o700);
  for (const entry of await readdir(path, { withFileTypes: true })) {
    if (entry.isDirectory()) await writable(join(path, entry.name));
  }
}

function entryModule(document) {
  const source = document
    .querySelector('script[type="module"][src]')
    ?.getAttribute("src");
  assert.ok(source, "the built document declares its application entry");
  return pathToFileURL(join(directory, "dist", source)).href;
}

test("a closed preview resumes its original cleanup channel after the Go Host is reconstructed", async () => {
  assert.ok(binary, "build the document command and set FRAME_DOCUMENT_BINARY");
  const state = await mkdtemp(join(tmpdir(), "frame-document-close-recovery-"));
  const documents = join(state, "documents");
  await mkdir(documents);
  await writeFile(
    join(documents, "hello.md"),
    "# Original preview\nRetained across Host reconstruction.\n",
  );
  const original = {
    window: globalThis.window,
    document: globalThis.document,
    HTMLElement: globalThis.HTMLElement,
    MutationObserver: globalThis.MutationObserver,
    fetch: globalThis.fetch,
  };
  let server,
    exited,
    origin,
    window,
    diagnostics = "",
    completed = false;
  const requests = [];
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
        join(directory, "dist"),
      ],
      { stdio: ["ignore", "ignore", "pipe"] },
    );
    exited = once(server, "exit");
    server.stderr.setEncoding("utf8");
    server.stderr.on("data", (chunk) => {
      current += chunk;
      diagnostics += chunk;
    });
    await until(
      () => /listening at http:\/\/[^;]+;/.test(current),
      () => diagnostics,
    );
    origin = /listening at (http:\/\/[^;]+);/.exec(current)[1];
  }
  try {
    await start();
    window = new Window({
      url: origin,
      settings: {
        disableCSSFileLoading: true,
        disableJavaScriptFileLoading: true,
        disableIframePageLoading: true,
      },
    });
    window.document.write(
      await readFile(join(directory, "dist/index.html"), "utf8"),
    );
    Object.assign(globalThis, {
      window,
      document: window.document,
      HTMLElement: window.HTMLElement,
      MutationObserver: window.MutationObserver,
      fetch: (input, init) => {
        const url = new URL(input, origin);
        requests.push({
          path: url.pathname,
          body: init?.body ? JSON.parse(init.body) : undefined,
        });
        return original.fetch(url, init);
      },
    });
    await import(entryModule(window.document) + "?close-host-recovery");
    const element = (id) => window.document.getElementById(id);
    async function click(id) {
      element(id).click();
      await until(
        () => !element(id).disabled,
        () => element("error").textContent + diagnostics,
      );
    }
    element("token").value = (
      await readFile(join(state, "application-token"), "utf8")
    ).trim();
    await click("connect");
    assert.equal(element("error").textContent, "");
    element("task").value = "close-recovery-original";
    await click("convert");
    assert.equal(element("error").textContent, "");
    assert.ok(window.document.querySelector("iframe.document-preview"));
    const address = new URL(origin).host;
    server.kill("SIGKILL");
    await exited;
    await start(address);

    await click("close");
    assert.match(element("error").textContent, /release did not complete/);
    assert.equal(element("reload").disabled, true);
    assert.equal(element("reconnect").disabled, true);
    assert.equal(
      window.document.querySelector("iframe.document-preview"),
      null,
    );
    await click("close");
    assert.equal(element("error").textContent, "");
    assert.equal(
      element("connection-state").textContent,
      "Original Realm released and acknowledged",
    );
    assert.equal(element("reload").disabled, false);
    assert.equal(
      requests.filter(({ path }) => path === "/api/realm/open").length,
      1,
    );
    assert.equal(
      requests.filter(({ path }) => path === "/api/preview/activate").length,
      1,
    );
    assert.equal(
      requests.filter(({ path }) => path === "/api/realm/resume").length,
      1,
    );
    const proofs = requests
      .filter(
        ({ path, body }) =>
          path === "/frame/control" && body?.method === "prove",
      )
      .map(({ body }) => body.input);
    assert.ok(proofs.length >= 2);
    assert.ok(
      proofs.some((proof, index) =>
        proofs
          .slice(index + 1)
          .some((later) => later.proofId === proof.proofId),
      ),
    );
    for (const proof of proofs)
      assert.deepEqual(
        proof,
        proofs.find((original) => original.proofId === proof.proofId),
      );
    completed = true;
  } finally {
    if (server?.exitCode === null)
      server.kill(completed ? "SIGINT" : "SIGKILL");
    if (exited) await exited;
    await window?.happyDOM.close();
    Object.assign(globalThis, original);
    await writable(state);
    await rm(state, { recursive: true, force: true });
  }
});
