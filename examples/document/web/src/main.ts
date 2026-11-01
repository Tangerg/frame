import { Lifetime, Point, type Unit } from "@frame/sdk";
import { parseJSON, projectNumber, stringifyJSON } from "@frame/sdk/json";
import {
  ControlPump,
  HTTPControl,
  RealmModules,
  Runtime,
  select,
  type Use,
} from "@frame/sdk/browser";
import type { ContractRef, Execution } from "@frame/sdk/protocol";
import type {
  Converted,
  Credentials,
  PreviewMetadata,
  TaskView,
} from "./api.generated.ts";
import { readLimited } from "./read.ts";
import "./style.css";

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
let credentials: Credentials | undefined;
let runtime: Runtime | undefined;
let control: HTTPControl | undefined;
let execution: Execution | undefined;
let point: Point<PreviewMetadata, Preview> | undefined;
let mounted: Use | undefined;
let pumpAbort: AbortController | undefined;
let pumpWork: Promise<void> | undefined;
let opening: { operationId: string; resumeToken: string } | undefined;
let realmReleased = false;

async function api<T>(path: string, input?: unknown): Promise<T> {
  return transport.run(async (signal) => {
    const response = await fetch(path, {
      method: input === undefined ? "GET" : "POST",
      signal,
      redirect: "error",
      credentials: "omit",
      headers: {
        Authorization: `Bearer ${tokenInput.value}`,
        "Content-Type": "application/json",
      },
      ...(input === undefined ? {} : { body: stringifyJSON(input) }),
    });
    const text = new TextDecoder("utf-8", { fatal: true }).decode(
      await readLimited(response, 32 * 1024 * 1024),
    );
    const value = parseJSON(text) as T & { error?: string };
    if (!response.ok)
      throw new Error(value.error ?? "application request failed");
    return value;
  });
}

function hostControl(session: Credentials): HTTPControl {
  return new HTTPControl({
    endpoint: new URL("/frame/control", window.location.href).href,
    credential: session.token,
    maximumMessageBytes: 8 * 1024 * 1024,
    lifetime: transport,
  });
}

async function connect(): Promise<void> {
  if (runtime)
    throw new Error(
      runtime.connection === "closed"
        ? "reload this page before opening another Realm"
        : "the current Realm already owns this page",
    );
  opening ??= {
    operationId: crypto.randomUUID(),
    resumeToken: crypto.randomUUID() + crypto.randomUUID(),
  };
  credentials = await api<Credentials>("/api/realm/open", opening);
  control = hostControl(credentials);
  const contracts = await api<{ previews: ContractRef }>("/api/contracts");
  point = new Point<PreviewMetadata, Preview>(contracts.previews);
  const modules = new RealmModules(async (unit, signal) => {
    const response = await fetch(unit.module.path, {
      signal,
      credentials: "omit",
      redirect: "error",
    });
    if (!response.ok) throw new Error("approved module is unavailable");
    const bytes = await readLimited(response, 1024 * 1024);
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
  pumpAbort = new AbortController();
  const pump = new ControlPump(runtime, control, {
    intervalMilliseconds: 100,
    onError: report,
  });
  pumpWork = pump.run(pumpAbort.signal);
  await api("/api/preview/activate", credentials);
  element("connection-state").textContent =
    `Realm ${credentials.realm.id.slice(0, 12)} / Host epoch ${credentials.authority.epoch}`;
}

async function preview(task: TaskView): Promise<void> {
  if (!task.result || !runtime || !control || !point || !execution) return;
  if (mounted) {
    const outcome = await mounted.close();
    if (outcome.status !== "released")
      throw new Error("previous preview has not released");
    await runtime.flushProofs();
    mounted = undefined;
  }
  const snapshot = await control.snapshot([point.ref.name]);
  try {
    const choice = select(
      snapshot,
      point,
      "documents.html-preview-priority/1",
      (choices) =>
        [...choices]
          .filter((choice) => choice.metadata.format === "html")
          .sort((a, b) => b.metadata.priority - a.metadata.priority)
          .slice(0, 1),
      (value) => {
        const metadata = value as { format?: unknown; priority?: unknown };
        if (typeof metadata.format !== "string")
          throw new TypeError("invalid preview metadata");
        return {
          format: metadata.format,
          priority: projectNumber(metadata.priority),
        };
      },
    );
    const use = await runtime.acquire({
      operationId: crypto.randomUUID(),
      snapshotId: snapshot.id,
      execution,
      selections: [choice],
    });
    mounted = use;
    const selected = snapshot.points[0]?.candidates.find(
      (candidate) => candidate.token === choice.candidates[0],
    );
    if (!selected) throw new Error("no approved preview is available");
    const theme = await api<{ css: string }>("/api/theme");
    const object = use.objects.find(
      (object) =>
        object.export.name === selected.source.export &&
        object.export.contract.digest === point!.ref.digest,
    );
    if (!object)
      throw new Error("the original Use does not own the selected preview");
    await use.with(object, point, (preview) =>
      preview.mount(
        element("preview"),
        task.result!.converted,
        theme.css,
        use.lifetime,
      ),
    );
  } catch (error) {
    try {
      await control.closeSnapshot(snapshot.id);
    } catch (closeFailure) {
      throw new AggregateError(
        [error, closeFailure],
        "snapshot selection and cleanup failed",
      );
    }
    throw error;
  }
}

function show(task: TaskView): void {
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
        button.disabled = false;
      });
  });
}

action("connect", connect);
action("queue", async () => {
  show(
    await api<TaskView>("/api/queue", { id: taskId(), path: pathInput.value }),
  );
});
action("run", async () => {
  const task = await api<TaskView>("/api/run", { id: taskId() });
  show(task);
  await preview(task);
});
action("convert", async () => {
  await api("/api/queue", { id: taskId(), path: pathInput.value });
  const task = await api<TaskView>("/api/run", { id: taskId() });
  show(task);
  await preview(task);
});
action("observe", async () => {
  show(await api<TaskView>(`/api/tasks/${encodeURIComponent(taskId())}`));
});
action("cancel", async () => {
  await api("/api/cancel", { id: taskId() });
  show(await api<TaskView>(`/api/tasks/${encodeURIComponent(taskId())}`));
});
action("reconnect", async () => {
  if (!credentials || !runtime) throw new Error("no original Realm exists");
  pumpAbort?.abort();
  await pumpWork;
  credentials = await api<Credentials>("/api/realm/resume", {
    realm: credentials.realm,
    resumeToken: credentials.resumeToken,
  });
  control = hostControl(credentials);
  await runtime.reconnect(control);
  pumpAbort = new AbortController();
  pumpWork = new ControlPump(runtime, control, {
    intervalMilliseconds: 100,
    onError: report,
  }).run(pumpAbort.signal);
  element("connection-state").textContent =
    `Original Realm: ${runtime.connection}`;
});
action("close", async () => {
  if (!credentials || !runtime) return;
  await api("/api/realm/seal", { token: credentials.token });
  pumpAbort?.abort();
  await pumpWork;
  const outcomes = await runtime.close();
  if (
    outcomes.some((outcome) => outcome.status !== "released") ||
    runtime.pendingProofs.length
  )
    throw new Error(
      "original resources or proof acknowledgements are still pending",
    );
  await api("/api/recover", {});
  realmReleased = true;
  credentials = undefined;
  mounted = undefined;
  execution = undefined;
  opening = undefined;
  element<HTMLButtonElement>("connect").disabled = true;
  element<HTMLButtonElement>("reload").disabled = false;
  element("connection-state").textContent =
    "Original Realm released and acknowledged";
});

action("reload", async () => {
  if (!runtime) return;
  await runtime.reload(() => window.location.reload());
});

window.addEventListener("beforeunload", (event) => {
  if (runtime && !realmReleased) event.preventDefault();
});
