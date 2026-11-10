import { FrameError, type CloseOutcome, type Lifetime } from "../index.js";
import { readResponseBytes } from "./response.js";
import { potentiallyTrustworthy } from "./trust.js";
import { parseJSON, stringifyJSON } from "../json.js";
import {
  CONTROL_MUTATIONS,
  decodeControlOutput,
  isControlProblem,
  type ControlInputs,
  type ControlOutputs,
} from "../protocol.generated.js";
import type {
  AcquireRequest,
  CloseProof,
  LocalPlan,
  PlanAdmission,
  Poll,
  ProofAcknowledgement,
  PublicationRequest,
  PublicationResult,
  ReadyReport,
  ReconnectRequest,
  ReconnectResult,
  RouteGrant,
  Snapshot,
  SnapshotRequest,
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

const mutations = new Set<string>(CONTROL_MUTATIONS);

export class HTTPControl implements HostControl {
  readonly #endpoint: string;
  #credential: string;
  readonly #maximumMessageBytes: number;
  readonly #lifetime: Lifetime;
  readonly #fetch: typeof fetch;

  constructor(options: HTTPControlOptions) {
    const {
      endpoint: endpointValue,
      credential,
      maximumMessageBytes,
      lifetime,
      fetch: fetchRequest,
    } = options;
    const endpoint = new URL(endpointValue);
    if (
      !["https:", "http:"].includes(endpoint.protocol) ||
      !potentiallyTrustworthy(endpoint) ||
      endpoint.username ||
      endpoint.password ||
      endpoint.hash ||
      typeof credential !== "string" ||
      !credential
    )
      throw new TypeError("control endpoint and realm credential are required");
    if (!Number.isSafeInteger(maximumMessageBytes) || maximumMessageBytes < 1)
      throw new TypeError(
        "control message capacity must be a positive safe integer",
      );
    if (fetchRequest !== undefined && typeof fetchRequest !== "function")
      throw new TypeError("control fetch must be a function");
    this.#endpoint = endpoint.href;
    this.#credential = credential;
    this.#maximumMessageBytes = maximumMessageBytes;
    this.#lifetime = lifetime.branch("authenticated control channel");
    const nativeFetch = globalThis.fetch;
    this.#fetch =
      fetchRequest && fetchRequest !== nativeFetch
        ? fetchRequest.bind(options)
        : nativeFetch.bind(globalThis);
    this.#lifetime.cleanup(() => {
      this.#credential = "";
    }, "control-credential");
  }

  async #request<K extends keyof ControlInputs>(
    method: K,
    input: ControlInputs[K],
    signal?: AbortSignal,
  ): Promise<ControlOutputs[K]> {
    const body = stringifyJSON({ method, input });
    if (new TextEncoder().encode(body).byteLength > this.#maximumMessageBytes)
      return Promise.reject(
        new FrameError(
          "capacity",
          "control request capacity exceeded before transmission",
        ),
      );
    return this.#lifetime.run((owner) =>
      this.#send(
        method,
        body,
        signal ? AbortSignal.any([owner, signal]) : owner,
      ),
    );
  }

  async #send<K extends keyof ControlInputs>(
    method: K,
    body: string,
    signal: AbortSignal,
  ): Promise<ControlOutputs[K]> {
    const mutating = mutations.has(method);
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
      if (isControlProblem(value))
        throw new FrameError(value.code, value.message);
      throw new FrameError(
        mutating ? "outcome_unknown" : "unavailable",
        "control operation was not confirmed",
      );
    }
    try {
      return decodeControlOutput(method, value);
    } catch (cause) {
      throw new FrameError(
        mutating ? "outcome_unknown" : "unavailable",
        "control response does not match its declared message",
        { cause },
      );
    }
  }

  close(signal?: AbortSignal): Promise<CloseOutcome> {
    return this.#lifetime.close(signal);
  }

  admitPlan(plan: LocalPlan, signal?: AbortSignal): Promise<PlanAdmission> {
    return this.#request("admitPlan", plan, signal);
  }
  ready(report: ReadyReport, signal?: AbortSignal): Promise<PublicationResult> {
    return this.#request("ready", report, signal);
  }
  acquire(request: AcquireRequest, signal?: AbortSignal): Promise<UseGrant> {
    return this.#request("acquire", request, signal);
  }
  validateUse(grant: UseGrant, signal?: AbortSignal): Promise<void> {
    return this.#request("validateUse", grant, signal);
  }
  validateRoute(grant: RouteGrant, signal?: AbortSignal): Promise<void> {
    return this.#request("validateRoute", grant, signal);
  }
  snapshot(request: SnapshotRequest, signal?: AbortSignal): Promise<Snapshot> {
    return this.#request("snapshot", request, signal);
  }
  closeSnapshot(snapshotId: string, signal?: AbortSignal): Promise<void> {
    return this.#request("closeSnapshot", snapshotId, signal);
  }
  publish(
    request: PublicationRequest,
    signal?: AbortSignal,
  ): Promise<PublicationResult> {
    return this.#request("publish", request, signal);
  }
  publication(
    operationId: string,
    signal?: AbortSignal,
  ): Promise<PublicationResult> {
    return this.#request("publication", operationId, signal);
  }
  withdraw(
    request: Withdrawal,
    signal?: AbortSignal,
  ): Promise<PublicationResult> {
    return this.#request("withdraw", request, signal);
  }
  prove(
    proof: CloseProof,
    signal?: AbortSignal,
  ): Promise<ProofAcknowledgement> {
    return this.#request("prove", proof, signal);
  }
  reconnect(
    request: ReconnectRequest,
    signal?: AbortSignal,
  ): Promise<ReconnectResult> {
    return this.#request("reconnect", request, signal);
  }
  poll(signal?: AbortSignal): Promise<Poll> {
    return this.#request("poll", {}, signal);
  }
}
