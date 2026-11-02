import { FrameError, type Lifetime } from "../index.js";
import { readResponseBytes } from "./response.js";
import { parseJSON, stringifyJSON } from "../json.js";
import {
  CONTROL_ERROR_CODES,
  CONTROL_MUTATIONS,
  type ControlInputs,
  type ControlOutputs,
} from "../protocol.generated.js";
import type {
  AcquireRequest,
  CloseProof,
  LocalPlan,
  Poll,
  ProofAcknowledgement,
  PublicationRequest,
  PublicationResult,
  ReadyReport,
  ReconnectRequest,
  ReconnectResult,
  RouteGrant,
  Snapshot,
  UseGrant,
  Withdrawal,
} from "../protocol.generated.js";
import type { HostControl } from "./control.js";

export interface HTTPControlOptions {
  readonly endpoint: string;
  readonly credential: string;
  readonly maximumMessageBytes: number;
  readonly lifetime: Lifetime;
  readonly fetch?: typeof fetch;
}

const codes = new Set<string>(CONTROL_ERROR_CODES);
const mutations = new Set<string>(CONTROL_MUTATIONS);

export class HTTPControl implements HostControl {
  readonly #endpoint: string;
  #credential: string;
  readonly #maximumMessageBytes: number;
  readonly #lifetime: Lifetime;
  readonly #fetch: typeof fetch;

  constructor(options: HTTPControlOptions) {
    const endpoint = new URL(options.endpoint);
    if (
      !["https:", "http:"].includes(endpoint.protocol) ||
      endpoint.username ||
      endpoint.password ||
      endpoint.hash ||
      !options.credential
    )
      throw new TypeError("control endpoint and realm credential are required");
    if (
      !Number.isSafeInteger(options.maximumMessageBytes) ||
      options.maximumMessageBytes < 1
    )
      throw new TypeError(
        "control message capacity must be a positive safe integer",
      );
    this.#endpoint = endpoint.href;
    this.#credential = options.credential;
    this.#maximumMessageBytes = options.maximumMessageBytes;
    this.#lifetime = options.lifetime;
    options.lifetime.cleanup(() => {
      this.#credential = "";
    }, "control-credential");
    this.#fetch = (options.fetch ?? globalThis.fetch).bind(globalThis);
  }

  async #request<K extends keyof ControlInputs>(
    method: K,
    input: ControlInputs[K],
  ): Promise<ControlOutputs[K]> {
    const body = stringifyJSON({ method, input });
    if (new TextEncoder().encode(body).byteLength > this.#maximumMessageBytes)
      return Promise.reject(
        new FrameError(
          "capacity",
          "control request capacity exceeded before transmission",
        ),
      );
    return this.#lifetime.run((signal) =>
      this.#send<ControlOutputs[K]>(body, signal, mutations.has(method)),
    );
  }

  async #send<T>(
    body: string,
    signal: AbortSignal,
    mutating: boolean,
  ): Promise<T> {
    let response: Response;
    try {
      response = await this.#fetch(this.#endpoint, {
        method: "POST",
        redirect: "error",
        credentials: "omit",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${this.#credential}`,
        },
        body,
        signal,
      });
    } catch (cause) {
      throw new FrameError(
        mutating ? "outcome_unknown" : "unavailable",
        "control transport did not return a response",
        { cause },
      );
    }
    let value: unknown;
    try {
      value = parseJSON(
        new TextDecoder("utf-8", { fatal: true }).decode(
          await readResponseBytes(response, this.#maximumMessageBytes),
        ),
      );
    } catch (cause) {
      throw new FrameError(
        mutating ? "outcome_unknown" : "unavailable",
        "control response is incomplete or malformed",
        { cause },
      );
    }
    if (!response.ok) {
      const problem = value as {
        code?: (typeof CONTROL_ERROR_CODES)[number];
        message?: string;
      } | null;
      if (
        problem &&
        problem.code &&
        codes.has(problem.code) &&
        typeof problem.message === "string"
      )
        throw new FrameError(problem.code, problem.message);
      throw new FrameError(
        mutating ? "outcome_unknown" : "unavailable",
        "control operation was not confirmed",
      );
    }
    return value as T;
  }

  validatePlan(plan: LocalPlan): Promise<void> {
    return this.#request("validatePlan", plan);
  }
  ready(report: ReadyReport): Promise<PublicationResult> {
    return this.#request("ready", report);
  }
  acquire(request: AcquireRequest): Promise<UseGrant> {
    return this.#request("acquire", request);
  }
  validateUse(grant: UseGrant): Promise<void> {
    return this.#request("validateUse", grant);
  }
  validateRoute(grant: RouteGrant): Promise<void> {
    return this.#request("validateRoute", grant);
  }
  snapshot(points: readonly string[]): Promise<Snapshot> {
    return this.#request("snapshot", points);
  }
  closeSnapshot(snapshotId: string): Promise<void> {
    return this.#request("closeSnapshot", snapshotId);
  }
  publish(request: PublicationRequest): Promise<PublicationResult> {
    return this.#request("publish", request);
  }
  publication(operationId: string): Promise<PublicationResult> {
    return this.#request("publication", operationId);
  }
  withdraw(request: Withdrawal): Promise<PublicationResult> {
    return this.#request("withdraw", request);
  }
  prove(proof: CloseProof): Promise<ProofAcknowledgement> {
    return this.#request("prove", proof);
  }
  reconnect(request: ReconnectRequest): Promise<ReconnectResult> {
    return this.#request("reconnect", request);
  }
  poll(): Promise<Poll> {
    return this.#request("poll", {});
  }
}
