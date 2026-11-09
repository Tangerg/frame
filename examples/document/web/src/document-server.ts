import { Lifetime } from "@frame/sdk";
import { HTTPControl, readResponseBytes } from "@frame/sdk/browser";
import { parseJSON, projectNumber, stringifyJSON } from "@frame/sdk/json";
import type { Credentials } from "./api.generated.ts";

const maximumApplicationResponseBytes = 32 * 1024 * 1024;

export class DocumentServer {
  readonly #transport = new Lifetime("document HTTP transport");
  readonly #credential: () => string;
  readonly #origin: string;

  constructor(origin: string, credential: () => string) {
    this.#origin = origin;
    this.#credential = credential;
  }

  async request<T>(
    path: string,
    input?: unknown,
    wait?: AbortSignal,
  ): Promise<T> {
    return this.#transport.run(async (signal) => {
      const response = await fetch(path, {
        method: input === undefined ? "GET" : "POST",
        signal: wait ? AbortSignal.any([signal, wait]) : signal,
        redirect: "error",
        credentials: "omit",
        headers: {
          Authorization: `Bearer ${this.#credential()}`,
          "Content-Type": "application/json",
        },
        ...(input === undefined ? {} : { body: stringifyJSON(input) }),
      });
      const text = new TextDecoder("utf-8", { fatal: true }).decode(
        await readResponseBytes(response, maximumApplicationResponseBytes),
      );
      const value: unknown = parseJSON(text);
      wait?.throwIfAborted();
      if (!response.ok) {
        const message =
          value &&
          typeof value === "object" &&
          "error" in value &&
          typeof value.error === "string"
            ? value.error
            : "application request failed";
        throw new Error(message);
      }
      return value as T;
    });
  }

  control(session: Credentials): HTTPControl {
    return new HTTPControl({
      endpoint: new URL("/frame/control", this.#origin).href,
      credential: session.token,
      maximumMessageBytes: projectNumber(session.maxMessageBytes),
      lifetime: this.#transport,
    });
  }
}
