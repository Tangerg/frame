import { FrameError, Lifetime } from "@frame/sdk";
import { HTTPControl, readResponseBytes } from "@frame/sdk/browser";
import { parseJSON, projectNumber, stringifyJSON } from "@frame/sdk/json";
import {
  decodeHTTPOutput,
  decodeWireValue,
  httpOperations,
  httpRequestPath,
  type HTTPInputs,
  type HTTPOutputs,
  type Credentials,
} from "./api.generated.ts";

const maximumApplicationResponseBytes = 32 * 1024 * 1024;

export class DocumentServer {
  readonly #transport = new Lifetime("document HTTP transport");
  readonly #credential: () => string;
  readonly #baseURL: string;

  constructor(baseURL: string, credential: () => string) {
    this.#baseURL = baseURL;
    this.#credential = credential;
  }

  async request<K extends keyof HTTPInputs>(
    operation: K,
    input: NoInfer<HTTPInputs[K]>,
    wait?: AbortSignal,
  ): Promise<HTTPOutputs[K]> {
    const path = httpRequestPath(operation, input);
    const contract = httpOperations[operation];
    return this.#transport.run(async (signal) => {
      const failureCode =
        contract.method === "GET" ? "unavailable" : "outcome_unknown";
      let response: Response;
      let value: unknown;
      try {
        response = await fetch(new URL(path, this.#baseURL), {
          method: contract.method,
          signal: wait ? AbortSignal.any([signal, wait]) : signal,
          redirect: "error",
          credentials: "omit",
          headers: {
            Authorization: `Bearer ${this.#credential()}`,
            "Content-Type": "application/json",
          },
          ...(contract.method === "POST" ? { body: stringifyJSON(input) } : {}),
        });
        const text = new TextDecoder("utf-8", { fatal: true }).decode(
          await readResponseBytes(response, maximumApplicationResponseBytes),
        );
        value = parseJSON(text);
        wait?.throwIfAborted();
      } catch (cause) {
        throw new FrameError(
          failureCode,
          `application response was not observed: ${operation}`,
          { cause },
        );
      }
      if (!response.ok) {
        let failure;
        try {
          failure = decodeWireValue("apiFailure", value);
        } catch (cause) {
          throw new FrameError(
            failureCode,
            "application error response is invalid",
            { cause },
          );
        }
        throw new Error(failure.error);
      }
      try {
        if (response.status !== contract.status)
          throw new TypeError("application response status is invalid");
        return decodeHTTPOutput(operation, value);
      } catch (cause) {
        throw new FrameError(failureCode, "application response is invalid", {
          cause,
        });
      }
    });
  }

  control(session: Credentials): HTTPControl {
    return new HTTPControl({
      endpoint: new URL("/frame/control", this.#baseURL).href,
      credential: session.token,
      maximumMessageBytes: projectNumber(session.maxMessageBytes),
      lifetime: this.#transport,
    });
  }
}
