import { Lifetime, Point, type Unit } from "@frame/sdk";
import {
  compareJSONNumbers,
  isJSONInteger,
  isJSONNumber,
  parseJSON,
  projectNumber,
  stringifyJSON,
} from "@frame/sdk/json";
import {
  ControlPump,
  readResponseBytes,
  HTTPControl,
  RealmModules,
  Runtime,
  select,
  type Use,
} from "@frame/sdk/browser";
import type {
  AcquireRequest,
  ContractRef,
  Execution,
} from "@frame/sdk/protocol";
import type {
  Converted,
  Credentials,
  PreviewMetadata,
  TaskView,
} from "./api.generated.ts";
import { PreviewSnapshots } from "./preview-capture.ts";
import "./style.css";

const maximumApplicationResponseBytes = 32 * 1024 * 1024;
const maximumPreviewModuleBytes = 1024 * 1024;

interface Preview {
  mount(
    element: HTMLElement,
    document: Converted,
    theme: string,
    owner: Lifetime,
  ): void;
}
const element = <T extends HTMLElement>(id: string) =>
  document.getElementById(id) as T;
const tokenInput = element<HTMLInputElement>("token");
const taskInput = element<HTMLInputElement>("task");
const pathInput = element<HTMLInputElement>("path");
const transport = new Lifetime("document HTTP transport");
const previewActions = new Lifetime("preview application actions");
let credentials: Credentials | undefined;
let runtime: Runtime | undefined;
let control: HTTPControl | undefined;
let execution: Execution | undefined;
let point: Point<PreviewMetadata, Preview> | undefined;
let mounted: Use | undefined;
let pendingAdmission: AcquireRequest | undefined;
let previewSnapshots: PreviewSnapshots | undefined;
let pump: ControlPump | undefined;
let pumpWork: Promise<void> | undefined;
let opening: { operationId: string; resumeToken: string } | undefined;
let realmReleased = false;

async function api<T>(
  path: string,
  input?: unknown,
  wait?: AbortSignal,
): Promise<T> {
  return transport.run(async (signal) => {
    const response = await fetch(path, {
      method: input === undefined ? "GET" : "POST",
      signal: wait ? AbortSignal.any([signal, wait]) : signal,
      redirect: "error",
      credentials: "omit",
      headers: {
        Authorization: `Bearer ${tokenInput.value}`,
        "Content-Type": "application/json",
      },
      ...(input === undefined ? {} : { body: stringifyJSON(input) }),
    });
    const text = new TextDecoder("utf-8", { fatal: true }).decode(
      await readResponseBytes(response, maximumApplicationResponseBytes),
    );
    const value = parseJSON(text) as T & { error?: string };
    wait?.throwIfAborted();
    if (!response.ok)
      throw new Error(value.error ?? "application request failed");
    return value;
  });
}

function hostControl(session: Credentials): HTTPControl {
  return new HTTPControl({
    endpoint: new URL("/frame/control", window.location.href).href,
    credential: session.token,
    maximumMessageBytes: projectNumber(session.maxMessageBytes),
    lifetime: transport,
  });
}

async function connect(signal: AbortSignal): Promise<void> {
  if (runtime) return reconnect(signal);
  opening ??= {
    operationId: crypto.randomUUID(),
    resumeToken: crypto.randomUUID() + crypto.randomUUID(),
  };
  const session = await api<Credentials>("/api/realm/open", opening, signal);
  signal.throwIfAborted();
  credentials = session;
  control = hostControl(credentials);
  const contracts = await api<{ previews: ContractRef }>(
    "/api/contracts",
    undefined,
    signal,
  );
  signal.throwIfAborted();
  point = new Point<PreviewMetadata, Preview>(contracts.previews);
  previewSnapshots = new PreviewSnapshots(point.ref.name);
  const modules = new RealmModules(async (unit, signal) => {
    const response = await fetch(unit.module.path, {
      signal,
      credentials: "omit",
      redirect: "error",
    });
    if (!response.ok) throw new Error("approved module is unavailable");
    const bytes = await readResponseBytes(response, maximumPreviewModuleBytes);
    const digest = [
      ...new Uint8Array(await crypto.subtle.digest("SHA-256", bytes)),
    ]
      .map((value) => value.toString(16).padStart(2, "0"))
      .join("");
    if (digest !== unit.module.digest)
      throw new Error("module differs from its approved digest");
    let binary = "";
    for (const value of bytes) binary += String.fromCharCode(value);
    return {
      url: `data:text/javascript;base64,${btoa(binary)}`,
      digest,
      close() {},
    };
  });
  runtime = new Runtime({
    authority: credentials.authority,
    realm: credentials.realm,
    control,
    proofCapacity: 512,
    load: async (unit, owner, plan): Promise<Unit> => {
      execution = plan.execution;
      return modules.load(unit, owner, plan);
    },
    remote: async () => {
      throw new Error("this consumer has no remote import contract");
    },
  });
  pump = new ControlPump(runtime, {
    intervalMilliseconds: 100,
    preparationCapacity: 1,
    onError: report,
  });
  pumpWork = pump.run().catch(report);
  await activatePreview(signal);
}

async function activatePreview(signal: AbortSignal): Promise<void> {
  if (!runtime || !credentials || runtime.connection !== "connected")
    throw new Error("connect the original Realm before activating its preview");
  // Host activation joins the existing instance in this same Realm after a lost reply.
  await api("/api/preview/activate", credentials, signal);
  signal.throwIfAborted();
  if (!execution)
    throw new Error(
      "preview activation has not completed; reconnect its Realm",
    );
  element("connection-state").textContent =
    `Realm ${credentials.realm.id.slice(0, 12)} / Host epoch ${credentials.authority.epoch}`;
}

async function reconnect(signal: AbortSignal): Promise<void> {
  if (!credentials || !runtime) throw new Error("no original Realm exists");
  if (runtime.connection === "closed")
    throw new Error("reload this page before opening another Realm");
  const session = await api<Credentials>(
    "/api/realm/resume",
    {
      realm: credentials.realm,
      resumeToken: credentials.resumeToken,
    },
    signal,
  );
  signal.throwIfAborted();
  credentials = session;
  control = hostControl(credentials);
  await runtime.reconnect(control);
  signal.throwIfAborted();
  if (runtime.connection === "connected") {
    const owner = runtime,
      channel = control;
    await serializePreview(() => recoverPreview(owner, channel));
    await activatePreview(signal);
  }
  element("connection-state").textContent =
    `Original Realm: ${runtime.connection}`;
}

let pendingConnection: Promise<void> = Promise.resolve();

async function serializeConnection(
  work: (signal: AbortSignal) => Promise<void>,
): Promise<void> {
  const previous = pendingConnection;
  const completion = previewActions.run(async (signal) => {
    await previous;
    signal.throwIfAborted();
    await work(signal);
  });
  pendingConnection = completion.catch(() => {});
  await completion;
}

let pendingPreview: Promise<void> = Promise.resolve();

async function serializePreview(
  work: (signal: AbortSignal) => Promise<void>,
): Promise<void> {
  const previous = pendingPreview;
  const completion = previewActions.run(async (signal) => {
    await previous;
    signal.throwIfAborted();
    await work(signal);
  });
  pendingPreview = completion.catch(() => {});
  await completion;
}

function preview(task: TaskView): Promise<void> {
  return serializePreview((signal) => mountPreview(task, signal));
}

async function closeSnapshotAfterFailure(
  channel: HTTPControl,
  snapshots: PreviewSnapshots,
  failure: unknown,
): Promise<never> {
  try {
    await snapshots.close(channel);
  } catch (cleanup) {
    throw new AggregateError(
      [failure, cleanup],
      "preview selection and cleanup failed",
    );
  }
  throw failure;
}

async function acquirePreview(
  owner: Runtime,
  channel: HTTPControl,
  snapshots: PreviewSnapshots,
  request: AcquireRequest,
): Promise<Use> {
  try {
    const use = await owner.acquire(request);
    snapshots.consumed(request.snapshotId);
    pendingAdmission = undefined;
    return use;
  } catch (error) {
    // Reuse the exact Host operation if its grant may have been created.
    if (owner.hasPendingAcquisition(request.operationId)) {
      pendingAdmission = request;
      throw error;
    }
    pendingAdmission = undefined;
    return closeSnapshotAfterFailure(channel, snapshots, error);
  }
}

async function closeMounted(owner: Runtime): Promise<void> {
  if (!mounted) return;
  const outcome = await mounted.close();
  if (outcome.status !== "released")
    throw new Error("preview release is unconfirmed");
  await owner.flushProofs();
  mounted = undefined;
}

async function recoverPreview(
  owner: Runtime,
  channel: HTTPControl,
): Promise<void> {
  if (!previewSnapshots) return;
  if (pendingAdmission) {
    mounted = await acquirePreview(
      owner,
      channel,
      previewSnapshots,
      pendingAdmission,
    );
    await closeMounted(owner);
  }
  await previewSnapshots.close(channel);
}

async function mountPreview(
  task: TaskView,
  signal: AbortSignal,
): Promise<void> {
  if (task.id !== taskInput.value || !task.result) return;
  if (!runtime || !control || !point || !previewSnapshots)
    throw new Error(
      "connect the preview before displaying the converted document",
    );
  if (!execution)
    throw new Error(
      "preview activation has not completed; reconnect its Realm",
    );
  const owner = runtime,
    channel = control;
  await recoverPreview(owner, channel);
  await closeMounted(owner);
  signal.throwIfAborted();
  const snapshot = await previewSnapshots.capture(channel);
  let choice;
  try {
    choice = select(
      snapshot,
      point,
      "documents.html-preview-priority/1",
      (choices) =>
        [...choices]
          .filter((choice) => choice.metadata.format === "html")
          .sort((a, b) =>
            compareJSONNumbers(b.metadata.priority, a.metadata.priority),
          )
          .slice(0, 1),
      (value) => {
        const metadata = value as { format?: unknown; priority?: unknown };
        if (
          typeof metadata.format !== "string" ||
          !isJSONNumber(metadata.priority) ||
          !isJSONInteger(metadata.priority)
        )
          throw new TypeError("invalid preview metadata");
        return {
          format: metadata.format,
          priority: metadata.priority,
        };
      },
    );
  } catch (error) {
    return closeSnapshotAfterFailure(channel, previewSnapshots, error);
  }
  const use = await acquirePreview(owner, channel, previewSnapshots, {
    operationId: crypto.randomUUID(),
    snapshotId: snapshot.id,
    execution,
    selections: [choice],
  });
  mounted = use;
  try {
    signal.throwIfAborted();
    const selected = snapshot.points[0]?.candidates.find(
      (candidate) => candidate.token === choice.candidates[0],
    );
    if (!selected) throw new Error("no approved preview is available");
    const theme = await api<{ css: string }>("/api/theme", undefined, signal);
    signal.throwIfAborted();
    const object = use.objects.find(
      (object) =>
        object.export.name === selected.source.export &&
        object.export.contract.digest === point!.ref.digest,
    );
    if (!object)
      throw new Error("the original Use does not own the selected preview");
    await use.with(object, point, (preview) => {
      if (task.id === taskInput.value)
        preview.mount(
          element("preview"),
          task.result!.converted,
          theme.css,
          use.lifetime,
        );
    });
    if (task.id !== taskInput.value) await closeMounted(owner);
  } catch (error) {
    try {
      await closeMounted(owner);
    } catch (closeFailure) {
      throw new AggregateError(
        [error, closeFailure],
        "preview preparation and cleanup failed",
      );
    }
    throw error;
  }
}

function show(task: TaskView): void {
  if (task.id !== taskInput.value) return;
  element("status").textContent =
    `${task.id}: ${task.state}${task.failure ? ` — ${task.failure}` : ""}`;
  element("binding").textContent = task.binding.units
    .map(
      (unit) =>
        `${unit.source.installationId}/${unit.source.unitId}\n${unit.source.release.slice(0, 20)}`,
    )
    .join("\n\n");
  const list = element("diagnostics");
  list.replaceChildren();
  for (const item of task.result?.diagnostics ?? []) {
    const row = document.createElement("li");
    row.textContent = `${item.rule}: ${item.message}`;
    list.append(row);
  }
}
function taskId(): string {
  if (!taskInput.value) taskInput.value = crypto.randomUUID();
  return taskInput.value;
}
function report(error: unknown): void {
  if (previewActions.signal.aborted && error === previewActions.signal.reason)
    return;
  element("error").textContent =
    error instanceof Error ? error.message : String(error);
}
function action(id: string, work: () => Promise<void>): void {
  element<HTMLButtonElement>(id).addEventListener("click", () => {
    const button = element<HTMLButtonElement>(id);
    button.disabled = true;
    element("error").textContent = "";
    void work()
      .catch(report)
      .finally(() => {
        button.disabled =
          (id === "connect" || id === "reconnect") && previewActions.closing;
      });
  });
}

action("connect", () => serializeConnection(connect));
action("queue", async () => {
  show(
    await api<TaskView>("/api/queue", { id: taskId(), path: pathInput.value }),
  );
});
action("run", async () => {
  const command = { id: taskId() };
  const task = await api<TaskView>("/api/run", command);
  show(task);
  if (task.id === taskInput.value) await preview(task);
});
action("convert", async () => {
  const command = { id: taskId(), path: pathInput.value };
  await api("/api/queue", command);
  const task = await api<TaskView>("/api/run", { id: command.id });
  show(task);
  if (task.id === taskInput.value) await preview(task);
});
action("observe", async () => {
  show(await api<TaskView>(`/api/tasks/${encodeURIComponent(taskId())}`));
});
action("cancel", async () => {
  const command = { id: taskId() };
  await api("/api/cancel", command);
  show(await api<TaskView>(`/api/tasks/${encodeURIComponent(command.id)}`));
});
action("reconnect", () => serializeConnection(reconnect));
async function closePreview(): Promise<void> {
  const work = previewActions.beginClose();
  element<HTMLButtonElement>("connect").disabled = true;
  element<HTMLButtonElement>("reconnect").disabled = true;
  // Cancellation must precede joining work whose preparation or mount it releases.
  const settled = await Promise.allSettled([
    sealPreview(),
    pump?.close(),
    pumpWork,
    work.wait(),
  ]);
  const failures = settled.flatMap((result) =>
    result.status === "rejected" ? [result.reason] : [],
  );
  if (failures.length)
    throw new AggregateError(
      failures,
      "original preview release did not complete",
    );
  await api("/api/recover", {});
  realmReleased = true;
  credentials = undefined;
  mounted = undefined;
  pendingAdmission = undefined;
  previewSnapshots = undefined;
  execution = undefined;
  opening = undefined;
  element<HTMLButtonElement>("connect").disabled = true;
  element<HTMLButtonElement>("reload").disabled = false;
  element("connection-state").textContent =
    "Original Realm released and acknowledged";
}

async function sealPreview(): Promise<void> {
  // A lost opening reply still belongs to its original operation, even before Runtime exists.
  const session =
    credentials ??
    (opening ? await api<Credentials>("/api/realm/open", opening) : undefined);
  if (session) await api("/api/realm/seal", { token: session.token });
}
action("close", closePreview);

action("reload", async () => {
  if (runtime) await runtime.reload(() => window.location.reload());
  else window.location.reload();
});

window.addEventListener("beforeunload", (event) => {
  if ((runtime || opening) && !realmReleased) event.preventDefault();
});
