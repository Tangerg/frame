import type { Readable, Writable } from "node:stream";
import { parseJSON, stringifyJSON } from "@frame/sdk/json";
import wire from "./wire.json" with { type: "json" };
import { exactFields } from "./contract.js";

export class ProcessError extends Error {
  constructor(
    readonly code: string,
    message: string,
    options?: ErrorOptions,
  ) {
    super(message, options);
  }
}

function unknownOutcome(cause: unknown): ProcessError {
  return new ProcessError(
    wire.errors.unknown,
    "dispatched process request has no confirmed outcome",
    { cause },
  );
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

function sequence(id: unknown): bigint {
  if (typeof id !== "string" || !/^[1-9][0-9]{0,19}$/.test(id))
    throw new ProcessError(wire.errors.invalid, "invalid request sequence");
  const result = BigInt(id);
  if (result > 0xffffffffffffffffn)
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
    this.#ended.abort(reason);
    for (const call of this.#pending.values()) {
      call.dispose();
      call.reject(unknownOutcome(reason));
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

  async #send(message: unknown): Promise<void> {
    this.signal.throwIfAborted();
    const data = Buffer.from(stringifyJSON(message) + "\n");
    if (data.length > this.#limit)
      throw new ProcessError(
        wire.errors.capacity,
        "frame exceeds negotiated limit",
      );
    if (this.#writing.size >= 2 * wire.maxInFlight)
      throw new ProcessError(wire.errors.capacity, "output capacity exceeded");
    let failed!: (cause: unknown) => void;
    try {
      await new Promise<void>((resolve, reject) => {
        failed = reject;
        this.#writing.add(failed);
        this.output.write(data, (err) => (err ? reject(err) : resolve()));
      });
    } catch (cause) {
      throw unknownOutcome(cause);
    } finally {
      this.#writing.delete(failed);
    }
  }

  request(
    method: string,
    params: unknown,
    signal: AbortSignal,
  ): Promise<unknown> {
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
    const id = (++this.#next).toString();
    sequence(id);
    return new Promise((resolve, reject) => {
      const abort = () => {
        // Retain the slot until the original reply or EOF. A cancelled waiter
        // cannot reuse capacity while its remote operation is still running.
        reject(unknownOutcome(signal.reason));
        void this.#send({ method: wire.methods.cancel, params: { id } }).catch(
          (err) => this.stop(err),
        );
      };
      const dispose = () => signal.removeEventListener("abort", abort);
      this.#pending.set(id, { resolve, reject, dispose });
      signal.addEventListener("abort", abort, { once: true });
      this.#send({ id, method, params }).catch((error) => {
        this.#pending.delete(id);
        dispose();
        reject(error);
      });
    });
  }

  #receive(data: Buffer, dispatch: Dispatch): void {
    if (data.length + 1 > this.#limit) throw new RangeError("frame too large");
    const parsed = parseJSON(
      new TextDecoder("utf-8", { fatal: true }).decode(data),
    );
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed))
      throw new TypeError("expected wire object");
    const value = parsed as Record<string, unknown>;
    if (!("method" in value)) {
      const failed = "error" in value;
      exactFields(value, ["id", failed ? "error" : "result"]);
      sequence(value.id);
      const id = value.id as string;
      const call = this.#pending.get(id);
      if (!call) throw new TypeError("unknown response");
      let failure: ProcessError | undefined;
      if (failed) {
        exactFields(value.error, ["code", "message"]);
        if (
          typeof value.error.code !== "string" ||
          typeof value.error.message !== "string"
        )
          throw new TypeError("invalid failure");
        failure = new ProcessError(value.error.code, value.error.message);
      }
      this.#pending.delete(id);
      call.dispose();
      if (failure) call.reject(failure);
      else call.resolve(value.result);
      return;
    }
    if (value.method === wire.methods.cancel) {
      exactFields(value, ["method", "params"]);
      exactFields(value.params, ["id"]);
      sequence(value.params.id);
      this.#incoming
        .get(value.params.id as string)
        ?.abort(new ProcessError(wire.errors.cancelled, "request cancelled"));
      return;
    }
    exactFields(value, ["id", "method", "params"]);
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
      if (signal.aborted) throw unknownOutcome(signal.reason);
      reply = { id, result };
    } catch (error) {
      const reason =
        error instanceof ProcessError ? error : unknownOutcome(error);
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
          const end = chunk.indexOf(10, start);
          const part = chunk.subarray(start, end < 0 ? chunk.length : end);
          const offset = size;
          size += part.length;
          if (size + 1 > this.#limit) throw new RangeError("frame too large");
          part.copy(frame, offset);
          if (end < 0) break;
          this.#receive(frame.subarray(0, size), dispatch);
          size = 0;
          start = end + 1;
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
