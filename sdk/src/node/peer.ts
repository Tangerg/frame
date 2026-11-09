import type { Readable, Writable } from "node:stream";
import { cloneJSON, parseJSON, stringifyJSON } from "../json.js";
import wire from "./wire.json" with { type: "json" };
import { admitShape } from "./contract.js";
import shapes from "./shapes.generated.json" with { type: "json" };

type FailureOptions = ErrorOptions & { outcome?: "unknown" };

export class ProcessError extends Error {
  readonly outcomeUnknown: boolean;

  constructor(
    readonly code: string,
    message: string,
    options?: FailureOptions,
  ) {
    super(message, options);
    this.name = "ProcessError";
    this.outcomeUnknown =
      code === wire.errors.unknown || options?.outcome === "unknown";
    Object.freeze(this);
  }

  static afterDispatch(cause: unknown): ProcessError {
    let cancelled = false;
    try {
      cancelled =
        cause instanceof ProcessError
          ? cause.code === wire.errors.cancelled
          : cause instanceof Error && cause.name === "AbortError";
    } catch {
      // Diagnostics cannot prevent recording the original failure or closing its peer.
    }
    if (cancelled) return ProcessError.cancelledAfterDispatch(cause);
    return new ProcessError(
      wire.errors.unknown,
      "dispatched process request has no confirmed outcome",
      { cause },
    );
  }

  static cancelledAfterDispatch(cause: unknown): ProcessError {
    return new ProcessError(
      wire.errors.cancelled,
      "dispatched process request was cancelled without a confirmed outcome",
      { cause, outcome: "unknown" },
    );
  }

  static fromReply(code: string, message: string): ProcessError {
    if (!Object.values(wire.errors).includes(code))
      throw new TypeError("invalid process error code");
    return new ProcessError(code, message, {
      outcome: code === wire.errors.cancelled ? "unknown" : undefined,
    });
  }
}

type Dispatch = (
  method: string,
  params: unknown,
  signal: AbortSignal,
) => Promise<unknown>;
type Pending = {
  resolve(value: unknown): void;
  reject(reason: unknown): void;
  dispose(): void;
};

const maxCallWrites = wire.maxInFlight;
const maxReplyWrites = wire.maxInFlight;
// Node retains both the active write and buffered writes until their callbacks settle.
const maxUnsettledWrites = maxCallWrites + maxReplyWrites;
const maxRequestSequence = 0xffffffffffffffffn;
const maxRequestSequenceDigits = maxRequestSequence.toString().length;
const frameDelimiter = "\n";
const frameDelimiterBytes = Buffer.byteLength(frameDelimiter);

function sequence(id: unknown): bigint {
  if (
    typeof id !== "string" ||
    id.length > maxRequestSequenceDigits ||
    !/^[1-9][0-9]*$/.test(id)
  )
    throw new ProcessError(wire.errors.invalid, "invalid request sequence");
  const result = BigInt(id);
  if (result > maxRequestSequence)
    throw new RangeError("request sequence exhausted");
  return result;
}

export class Peer {
  readonly #pending = new Map<string, Pending>();
  readonly #incoming = new Map<string, AbortController>();
  readonly #active = new Set<Promise<void>>();
  readonly #ended = new AbortController();
  #next = 0n;
  #received = 0n;
  readonly #writing = new Set<(cause: unknown) => void>();
  #limit = wire.maxFrameBytes;

  constructor(
    readonly input: Readable,
    readonly output: Writable,
  ) {}

  get signal(): AbortSignal {
    return this.#ended.signal;
  }
  limit(bytes: number): void {
    if (!Number.isSafeInteger(bytes) || bytes < 1 || bytes > this.#limit)
      throw new ProcessError(wire.errors.invalid, "invalid frame bound");
    this.#limit = bytes;
  }

  stop(reason: unknown): void {
    if (this.signal.aborted) return;
    const failure = ProcessError.afterDispatch(reason);
    this.#ended.abort(reason);
    for (const call of this.#pending.values()) {
      call.dispose();
      call.reject(failure);
    }
    this.#pending.clear();
    for (const controller of this.#incoming.values()) controller.abort(reason);
    // A blocked write may never invoke its callback after peer EOF. Neither the
    // original handler nor process retirement may wait for remote consumption.
    for (const reject of this.#writing) reject(reason);
    this.#writing.clear();
    this.input.destroy();
    this.output.destroy();
  }

  #encode(message: unknown): Buffer {
    this.signal.throwIfAborted();
    const data = Buffer.from(stringifyJSON(message) + frameDelimiter);
    this.signal.throwIfAborted();
    if (data.length > this.#limit)
      throw new ProcessError(
        wire.errors.capacity,
        "frame exceeds negotiated limit",
      );
    if (this.#writing.size >= maxUnsettledWrites)
      throw new ProcessError(wire.errors.capacity, "output capacity exceeded");
    return data;
  }

  async #send(message: unknown): Promise<void> {
    await this.#write(this.#encode(message));
  }

  async #write(data: Buffer): Promise<void> {
    let failed!: (cause: unknown) => void;
    try {
      await new Promise<void>((resolve, reject) => {
        failed = reject;
        this.#writing.add(failed);
        this.output.write(data, (err) => (err ? reject(err) : resolve()));
      });
    } catch (cause) {
      throw ProcessError.afterDispatch(cause);
    } finally {
      this.#writing.delete(failed);
    }
  }

  #checkRequestAdmission(signal: AbortSignal): void {
    if (signal.aborted)
      throw new ProcessError(
        wire.errors.cancelled,
        "request cancelled before dispatch",
        { cause: signal.reason },
      );
    if (this.signal.aborted)
      throw new ProcessError(
        wire.errors.closed,
        "channel closed before dispatch",
        { cause: this.signal.reason },
      );
    if (this.#pending.size >= wire.maxInFlight)
      throw new ProcessError(wire.errors.capacity, "call capacity exceeded");
  }

  request(
    method: string,
    params: unknown,
    signal: AbortSignal,
  ): Promise<unknown> {
    this.#checkRequestAdmission(signal);
    // User getters may cancel, close the peer, or dispatch another request.
    // Capture their values before reserving an identity in wire order.
    const captured = { method, params: cloneJSON(params) };
    this.#checkRequestAdmission(signal);
    const next = this.#next + 1n;
    const id = next.toString();
    sequence(id);
    const data = this.#encode({ id, ...captured });
    this.#next = next;
    return new Promise((resolve, reject) => {
      const abort = () => {
        // Retain the slot until the original reply or EOF. A cancelled waiter
        // cannot reuse capacity while its remote operation is still running.
        reject(ProcessError.cancelledAfterDispatch(signal.reason));
        void this.#send({ method: wire.methods.cancel, params: { id } }).catch(
          (err) => this.stop(err),
        );
      };
      const dispose = () => signal.removeEventListener("abort", abort);
      this.#pending.set(id, { resolve, reject, dispose });
      signal.addEventListener("abort", abort, { once: true });
      void this.#write(data).catch((error) => this.stop(error));
    });
  }

  #receive(data: Buffer, dispatch: Dispatch): void {
    if (data.length + frameDelimiterBytes > this.#limit)
      throw new RangeError("frame too large");
    const parsed = parseJSON(
      new TextDecoder("utf-8", { fatal: true }).decode(data),
    );
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed))
      throw new TypeError("expected wire object");
    const value = parsed as Record<string, unknown>;
    if (!("method" in value)) this.#receiveResponse(value);
    else if (value.method === wire.methods.cancel)
      this.#receiveCancellation(value);
    else this.#receiveRequest(value, dispatch);
  }

  #receiveResponse(value: Record<string, unknown>): void {
    const failed = "error" in value;
    admitShape(value, failed ? shapes.failure : shapes.result);
    sequence(value.id);
    const id = value.id as string;
    const call = this.#pending.get(id);
    if (!call) throw new TypeError("unknown response");
    const failure = failed ? replyFailure(value.error) : undefined;
    this.#pending.delete(id);
    call.dispose();
    if (failure) call.reject(failure);
    else call.resolve(value.result);
  }

  #receiveCancellation(value: Record<string, unknown>): void {
    admitShape(value, shapes.cancel);
    admitShape(value.params, shapes.cancelled);
    sequence(value.params.id);
    this.#incoming
      .get(value.params.id as string)
      ?.abort(new ProcessError(wire.errors.cancelled, "request cancelled"));
  }

  #receiveRequest(value: Record<string, unknown>, dispatch: Dispatch): void {
    admitShape(value, shapes.request);
    const num = sequence(value.id);
    if (num <= this.#received || typeof value.method !== "string")
      throw new TypeError("invalid request order");
    this.#received = num;
    if (this.#incoming.size >= wire.maxInFlight) {
      void this.#send({
        id: value.id,
        error: {
          code: wire.errors.capacity,
          message: "call capacity exceeded",
        },
      }).catch((error) => this.stop(error));
      return;
    }
    const id = value.id as string;
    const controller = new AbortController();
    this.#incoming.set(id, controller);
    const task = this.#answer(
      id,
      value.method,
      value.params,
      controller.signal,
      dispatch,
    )
      .catch((error) => this.stop(error))
      .finally(() => {
        this.#active.delete(task);
      });
    this.#active.add(task);
  }

  async #answer(
    id: string,
    method: string,
    params: unknown,
    signal: AbortSignal,
    dispatch: Dispatch,
  ): Promise<void> {
    let reply: unknown;
    try {
      const result = await dispatch(method, params, signal);
      reply = { id, result };
    } catch (error) {
      const reason =
        error instanceof ProcessError
          ? error
          : ProcessError.afterDispatch(error);
      reply = { id, error: { code: reason.code, message: reason.message } };
    } finally {
      // Once the peer observes the reply it can immediately reuse the slot.
      // Queued writes have a separate bound from active request handlers.
      this.#incoming.delete(id);
    }
    await this.#send(reply);
  }

  async run(dispatch: Dispatch): Promise<void> {
    const interrupted = (error: Error) => this.stop(error);
    this.output.on("error", interrupted);
    const frame = Buffer.allocUnsafe(this.#limit);
    let size = 0;
    try {
      for await (const incoming of this.input) {
        const chunk = Buffer.isBuffer(incoming)
          ? incoming
          : Buffer.from(incoming);
        let start = 0;
        while (start < chunk.length) {
          const end = chunk.indexOf(frameDelimiter, start);
          const part = chunk.subarray(start, end < 0 ? chunk.length : end);
          const offset = size;
          size += part.length;
          if (size + frameDelimiterBytes > this.#limit)
            throw new RangeError("frame too large");
          part.copy(frame, offset);
          if (end < 0) break;
          this.#receive(frame.subarray(0, size), dispatch);
          size = 0;
          start = end + frameDelimiterBytes;
        }
      }
      if (size) throw new TypeError("truncated process frame");
    } finally {
      this.stop(new ProcessError(wire.errors.closed, "process channel closed"));
      await Promise.allSettled(this.#active);
      this.output.off("error", interrupted);
    }
  }
}

function replyFailure(error: unknown): ProcessError {
  admitShape(error, shapes.problem);
  if (typeof error.code !== "string" || typeof error.message !== "string")
    throw new TypeError("invalid failure");
  return ProcessError.fromReply(error.code, error.message);
}
