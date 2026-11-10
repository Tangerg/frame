import type { TaskView } from "./api.generated.ts";
import { DocumentServer } from "./document-server.ts";
import { PreviewConnection } from "./preview-connection.ts";
import "./style.css";

function element<T extends HTMLElement>(id: string): T {
  const found = document.getElementById(id);
  if (!found) throw new Error(`document interface is missing ${id}`);
  return found as T;
}

class DocumentWorkbench {
  readonly #token = element<HTMLInputElement>("token");
  readonly #task = element<HTMLInputElement>("task");
  readonly #path = element<HTMLInputElement>("path");
  readonly #server = new DocumentServer(
    window.location.href,
    () => this.#token.value,
  );
  readonly #preview = new PreviewConnection(this.#server, (error) =>
    this.#report(error),
  );

  constructor() {
    this.#action("connect", async () => {
      element("connection-state").textContent = await this.#preview.connect();
    });
    this.#action("reconnect", async () => {
      element("connection-state").textContent = await this.#preview.reconnect();
    });
    this.#action("queue", async () => {
      this.#show(
        await this.#server.request("queue", {
          id: this.#taskId(),
          path: this.#path.value,
        }),
      );
    });
    this.#action("run", () => this.#run(this.#taskId()));
    this.#action("convert", async () => {
      const command = { id: this.#taskId(), path: this.#path.value };
      await this.#server.request("queue", command);
      await this.#run(command.id);
    });
    this.#action("observe", async () => {
      this.#show(await this.#server.request("task", { id: this.#taskId() }));
    });
    this.#action("cancel", async () => {
      const id = this.#taskId();
      await this.#server.request("cancel", { id });
      this.#show(await this.#server.request("task", { id }));
    });
    this.#action("close", async () => {
      const released = this.#preview.close();
      element<HTMLButtonElement>("connect").disabled = true;
      element<HTMLButtonElement>("reconnect").disabled = true;
      await released;
      element<HTMLButtonElement>("reload").disabled = false;
      this.#showConnection();
    });
    this.#action("reload", async () =>
      this.#preview.reload(() => window.location.reload()),
    );
    window.addEventListener("beforeunload", (event) => {
      if (this.#preview.needsRelease) event.preventDefault();
    });
  }

  async #run(id: string): Promise<void> {
    const task = await this.#server.request("run", { id });
    this.#show(task);
    if (task.id === this.#task.value && task.result) {
      await this.#preview.mount(
        task.result.converted,
        element("preview"),
        () => task.id === this.#task.value,
      );
    }
  }

  #show(task: TaskView): void {
    if (task.id !== this.#task.value) return;
    element("status").textContent =
      `${task.id}: ${task.state}${task.runStatus ? " / " + task.runStatus : ""}${task.failure ? ` — ${task.failure}` : ""}`;
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

  #showConnection(): void {
    element("connection-state").textContent = this.#preview.description;
  }

  #taskId(): string {
    if (!this.#task.value) this.#task.value = crypto.randomUUID();
    return this.#task.value;
  }

  #report(error: unknown): void {
    const signal = this.#preview.signal;
    if (
      signal.aborted &&
      (error === signal.reason ||
        (error instanceof Error && error.cause === signal.reason))
    )
      return;
    element("error").textContent =
      error instanceof Error ? error.message : String(error);
  }

  #action(id: string, work: () => Promise<void>): void {
    const button = element<HTMLButtonElement>(id);
    button.addEventListener("click", () => {
      button.disabled = true;
      element("error").textContent = "";
      void work()
        .catch((error) => this.#report(error))
        .finally(() => {
          button.disabled =
            (id === "connect" || id === "reconnect") && this.#preview.closing;
        });
    });
  }
}

new DocumentWorkbench();
