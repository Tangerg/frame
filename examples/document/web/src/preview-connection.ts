import { Lifetime } from "@frame/sdk";
import type { ContractRef } from "@frame/sdk/protocol";
import type { Converted, Credentials } from "./api.generated.ts";
import { DocumentServer } from "./document-server.ts";
import { PreviewSession } from "./preview-session.ts";
import { SerialActions } from "./serial-actions.ts";

type ConnectionState =
  | { status: "idle" }
  | { status: "opening"; request: { operationId: string; resumeToken: string } }
  | { status: "opened"; credentials: Credentials }
  | { status: "resident"; session: PreviewSession }
  | { status: "released" };

export class PreviewConnection {
  readonly #server: DocumentServer;
  readonly #actions = new Lifetime("preview application actions");
  readonly #connections = new SerialActions(this.#actions);
  readonly #report: (error: unknown) => void;
  #state: ConnectionState = { status: "idle" };
  #closing?: Promise<void>;

  constructor(server: DocumentServer, report: (error: unknown) => void) {
    this.#server = server;
    this.#report = report;
  }

  get signal(): AbortSignal {
    return this.#actions.signal;
  }
  get closing(): boolean {
    return this.#actions.closing;
  }
  get needsRelease(): boolean {
    return this.#state.status !== "idle" && this.#state.status !== "released";
  }
  get description(): string {
    if (this.#state.status === "released")
      return "Original Realm released and acknowledged";
    if (this.#state.status === "resident")
      return this.#state.session.description;
    return "Preview is not connected";
  }

  connect(): Promise<string> {
    return this.#connections.run((signal) => this.#connect(signal));
  }

  reconnect(): Promise<string> {
    return this.#connections.run(async (signal) => {
      if (this.#state.status !== "resident")
        throw new Error("no original Realm exists");
      return this.#state.session.reconnect(signal);
    });
  }

  mount(
    document: Converted,
    target: HTMLElement,
    current: () => boolean,
  ): Promise<void> {
    if (this.#state.status !== "resident")
      throw new Error(
        "connect the preview before displaying the converted document",
      );
    return this.#state.session.mount(document, target, current);
  }

  close(): Promise<void> {
    if (this.#state.status === "released") return Promise.resolve();
    if (!this.#closing) {
      const completion = Promise.withResolvers<void>();
      const work = completion.promise.finally(() => {
        if (this.#closing === work) this.#closing = undefined;
      });
      this.#closing = work;
      this.#close().then(completion.resolve, completion.reject);
    }
    return this.#closing;
  }

  reload(reload: () => void): void {
    if (this.#state.status !== "released")
      throw new Error(
        "release and acknowledge the original Realm before reloading",
      );
    reload();
  }

  async #connect(signal: AbortSignal): Promise<string> {
    if (this.#state.status === "resident")
      return this.#state.session.reconnect(signal);
    if (this.#state.status === "released")
      throw new Error("reload this page before opening another Realm");
    if (this.#state.status === "idle") {
      this.#state = {
        status: "opening",
        request: {
          operationId: crypto.randomUUID(),
          resumeToken: crypto.randomUUID() + crypto.randomUUID(),
        },
      };
    }
    if (this.#state.status === "opening") {
      const credentials = await this.#server.request<Credentials>(
        "/api/realm/open",
        this.#state.request,
        signal,
      );
      signal.throwIfAborted();
      this.#state = { status: "opened", credentials };
    }
    const { credentials } = this.#state;
    const contracts = await this.#server.request<{ previews: ContractRef }>(
      "/api/contracts",
      undefined,
      signal,
    );
    signal.throwIfAborted();
    const session = new PreviewSession(
      this.#server,
      credentials,
      contracts.previews,
      this.#actions,
      this.#report,
    );
    this.#state = { status: "resident", session };
    await session.activate(signal);
    return session.description;
  }

  async #close(): Promise<void> {
    // Seal admission before joining activation or mounting work that needs its
    // original Runtime to close. A lost open reply retains the same operation.
    const exit = this.#actions.beginClose();
    const state = this.#state;
    const settled = await Promise.allSettled([
      state.status === "resident" ? state.session.close() : this.#seal(state),
      exit.wait(),
    ]);
    const failures = settled.flatMap((result) =>
      result.status === "rejected" ? [result.reason] : [],
    );
    if (failures.length)
      throw new AggregateError(
        failures,
        "original preview release did not complete",
      );
    await this.#server.request("/api/recover", {});
    this.#state = { status: "released" };
  }

  async #seal(
    state: Exclude<ConnectionState, { status: "resident" }>,
  ): Promise<void> {
    switch (state.status) {
      case "opening": {
        const credentials = await this.#server.request<Credentials>(
          "/api/realm/open",
          state.request,
        );
        await this.#server.request("/api/realm/seal", {
          token: credentials.token,
        });
        return;
      }
      case "opened": {
        const credentials = await this.#server.request<Credentials>(
          "/api/realm/resume",
          {
            realm: state.credentials.realm,
            resumeToken: state.credentials.resumeToken,
          },
        );
        await this.#server.request("/api/realm/seal", {
          token: credentials.token,
        });
        return;
      }
      case "idle":
      case "released":
        return;
    }
  }
}
