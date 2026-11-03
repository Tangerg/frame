/// <reference types="node" preserve="true" />
import { randomUUID } from "node:crypto";
import type { Readable, Writable } from "node:stream";
import { Lifetime, type Awaitable } from "../index.js";
import {
  Capability,
  exactFields,
  validToken,
  type ServiceContract,
  type Handlers,
  type Client,
  type RequestOptions,
  type CallContext,
} from "./contract.js";
import { Peer, ProcessError } from "./peer.js";
import wire from "./wire.json" with { type: "json" };

export {
  Capability,
  type CapabilityDescriptor,
  type MethodDescriptor,
} from "./contract.js";
export type {
  Client,
  RequestOptions,
  CallContext,
  Handlers,
} from "./contract.js";
export { ProcessError } from "./peer.js";

export const optional = <C extends ServiceContract>(contract: C) =>
  Object.freeze({ contract, cardinality: "optional" as const });
export const collection = <C extends ServiceContract>(contract: C) =>
  Object.freeze({ contract, cardinality: "many" as const });
type Import =
  | ServiceContract
  | {
      readonly contract: ServiceContract;
      readonly cardinality: "optional" | "many";
    };
type Imports = Readonly<Record<string, Import>>;
type Exports = Readonly<Record<string, ServiceContract>>;
type Imported<I extends Imports> = {
  readonly [K in keyof I]: I[K] extends ServiceContract
    ? Client<I[K]>
    : I[K] extends {
          contract: infer C extends ServiceContract;
          cardinality: "optional";
        }
      ? Client<C> | undefined
      : I[K] extends { contract: infer C extends ServiceContract }
        ? readonly Client<C>[]
        : never;
};

export interface Activation {
  start(job: (signal: AbortSignal) => Awaitable<void>): void;
}
export interface ProcessUnit<I extends Imports, E extends Exports> {
  id: string;
  imports: I;
  exports: E;
  setup(context: {
    readonly imports: Imported<I>;
    readonly lifetime: Lifetime;
    onActivate(start: (work: Activation) => Awaitable<void>): void;
  }): Awaitable<{ readonly [K in keyof E]: Handlers<E[K]> }>;
}

export function defineProcessUnit<
  const I extends Imports,
  const E extends Exports,
>(unit: ProcessUnit<I, E>): ProcessUnit<I, E> {
  if (!validToken(unit.id) || typeof unit.setup !== "function")
    throw new TypeError("invalid process unit");
  for (const [name, token] of Object.entries(unit.imports)) {
    if (
      !validToken(name) ||
      !(
        token instanceof Capability ||
        ("contract" in token &&
          token.contract instanceof Capability &&
          ["many", "optional"].includes(token.cardinality))
      )
    )
      throw new TypeError("invalid import declaration");
  }
  for (const [name, contract] of Object.entries(unit.exports)) {
    if (!validToken(name) || !(contract instanceof Capability))
      throw new TypeError("invalid export declaration");
  }
  return Object.freeze({
    ...unit,
    imports: Object.freeze({ ...unit.imports }),
    exports: Object.freeze({ ...unit.exports }),
  });
}

type Phase =
  "new" | "described" | "preparing" | "prepared" | "active" | "closed";
type Handler = (
  input: unknown,
  context: CallContext<unknown>,
) => Awaitable<unknown>;

// Each call owns its accepted deliveries and first failure. A producer's final
// result cannot revoke a rejection or bypass a consumer still using the chunk.
class StreamCall {
  readonly #stop = new AbortController();
  readonly signal: AbortSignal;
  #closed = false;
  #pending?: Promise<void>;
  #failure?: ProcessError;

  constructor(parent: AbortSignal) {
    this.signal = AbortSignal.any([parent, this.#stop.signal]);
  }

  #fail(cause: unknown): ProcessError {
    this.#failure ??= ProcessError.afterDispatch(cause);
    this.#stop.abort(this.#failure);
    return this.#failure;
  }

  deliver(
    wait: AbortSignal,
    consume: (signal: AbortSignal) => Awaitable<void>,
  ): Promise<void> {
    let delivery: Promise<void>;
    if (this.#closed) {
      delivery = Promise.reject(
        new ProcessError(wire.errors.closed, "stream is closed"),
      );
    } else if (this.#pending) {
      delivery = Promise.reject(
        this.#fail(new Error("concurrent chunk delivery")),
      );
    } else {
      const active = AbortSignal.any([this.signal, wait]);
      delivery = Promise.resolve()
        .then(async () => {
          try {
            active.throwIfAborted();
            await consume(active);
            active.throwIfAborted();
          } catch (cause) {
            throw this.#fail(cause);
          }
        })
        .finally(() => {
          this.#pending = undefined;
        });
      this.#pending = delivery;
    }
    // The call still observes a rejected delivery when its producer forgets to
    // await emit; an unhandled promise must not become a second failure path.
    void delivery.catch(() => {});
    return delivery;
  }

  async run<T>(invoke: (signal: AbortSignal) => Promise<T>): Promise<T> {
    let response:
      { status: "returned"; value: T } | { status: "failed"; cause: unknown };
    try {
      response = { status: "returned", value: await invoke(this.signal) };
    } catch (cause) {
      response = { status: "failed", cause };
      this.#stop.abort(cause);
    }
    this.#closed = true;
    const pending = this.#pending;
    if (pending) {
      this.#fail(
        new Error(
          "stream ended with pending delivery",
          response.status === "failed" ? { cause: response.cause } : undefined,
        ),
      );
      await pending.catch(() => {});
    }
    this.#stop.abort(new ProcessError(wire.errors.closed, "stream is closed"));
    if (this.#failure) throw this.#failure;
    if (response.status === "failed") throw response.cause;
    return response.value;
  }
}

function digest(value: unknown): value is string {
  return typeof value === "string" && /^[a-f0-9]{64}$/.test(value);
}

function checkPortableRef(
  value: unknown,
): asserts value is Record<string, unknown> {
  exactFields(value, ["name", "version", "digest", "kind", "mode"]);
  if (
    !validToken(value.name) ||
    !validToken(value.version) ||
    !digest(value.digest) ||
    value.mode !== "portable" ||
    (value.kind !== "service" && value.kind !== "point")
  )
    throw new ProcessError(
      wire.errors.contract,
      "invalid portable contract identity",
    );
}

class Instance<I extends Imports, E extends Exports> {
  readonly lifetime = new Lifetime("process unit");
  readonly #streams = new Map<
    string,
    (chunk: unknown, wait: AbortSignal) => Promise<void>
  >();
  #phase: Phase = "new";
  #activate?: (work: Activation) => Awaitable<void>;
  #exports: Readonly<Record<string, Record<string, Handler>>> = Object.freeze(
    {},
  );
  constructor(
    readonly unit: ProcessUnit<I, E>,
    readonly peer: Peer,
  ) {}

  #expect(expected: Phase): void {
    if (this.#phase !== expected)
      throw new ProcessError(
        wire.errors.closed,
        "operation outside its admitted phase",
      );
  }

  #client(handle: string, service: ServiceContract) {
    return Object.freeze(
      Object.fromEntries(
        service.descriptor.methods.map((descriptor) => {
          const method = service.method(descriptor.name);
          return [
            descriptor.name,
            async (input: unknown, options: RequestOptions = {}) => {
              this.#expect("active");
              const commandID = options.commandID;
              method.validateInvocation(input, commandID);
              const signal = options.signal
                ? AbortSignal.any([this.lifetime.signal, options.signal])
                : this.lifetime.signal;
              if (Boolean(method.validateChunk) !== Boolean(options.onChunk))
                throw new TypeError(
                  "stream receiver differs from the declared method",
                );
              const stream = options.onChunk ? randomUUID() : undefined;
              const call = stream ? new StreamCall(signal) : undefined;
              if (stream && call) {
                this.#streams.set(stream, (chunk, wait) =>
                  call.deliver(wait, async () => {
                    method.validateChunk!(chunk);
                    await options.onChunk!(chunk);
                  }),
                );
              }
              const invoke = async (active: AbortSignal) => {
                const result = await this.peer.request(
                  wire.methods.call,
                  { handle, member: descriptor.name, input, stream, commandID },
                  active,
                );
                try {
                  method.validateOutput(result);
                } catch (cause) {
                  throw ProcessError.afterDispatch(cause);
                }
                return result;
              };
              try {
                return await this.lifetime.run(() =>
                  call ? call.run(invoke) : invoke(signal),
                );
              } finally {
                if (stream) this.#streams.delete(stream);
              }
            },
          ];
        }),
      ),
    );
  }

  #imports(raw: unknown): Imported<I> {
    exactFields(raw, Object.keys(this.unit.imports));
    const values: Record<string, unknown> = Object.create(null);
    for (const [slot, spec] of Object.entries(this.unit.imports)) {
      const bindings = raw[slot];
      if (!Array.isArray(bindings))
        throw new TypeError("expected approved bindings");
      const service = "contract" in spec ? spec.contract : spec;
      const mode = "cardinality" in spec ? spec.cardinality : "one";
      if (
        (mode === "one" && bindings.length !== 1) ||
        (mode === "optional" && bindings.length > 1)
      )
        throw new ProcessError(
          wire.errors.contract,
          "import cardinality mismatch",
        );
      const clients = bindings.map((binding: unknown) => {
        exactFields(binding, ["handle", "source"], ["valueContract"]);
        exactFields(binding.source, [
          "installationId",
          "release",
          "unitId",
          "export",
          "contract",
        ]);
        if (
          !validToken(binding.handle) ||
          !validToken(binding.source.installationId) ||
          !validToken(binding.source.unitId) ||
          !validToken(binding.source.export) ||
          !digest(binding.source.release)
        )
          throw new ProcessError(
            wire.errors.contract,
            "invalid approved provider identity",
          );
        const ref = binding.source.contract;
        checkPortableRef(ref);
        const point = ref.kind === "point";
        if (point !== Object.hasOwn(binding, "valueContract"))
          throw new ProcessError(
            wire.errors.contract,
            "invalid provider value contract",
          );
        const value = point ? binding.valueContract : ref;
        checkPortableRef(value);
        if (
          value.kind !== "service" ||
          value.digest !== service.identity ||
          value.name !== service.descriptor.reference
        )
          throw new ProcessError(
            wire.errors.contract,
            "approved provider differs from required service",
          );
        return this.#client(binding.handle, service);
      });
      values[slot] = mode === "many" ? Object.freeze(clients) : clients[0];
    }
    return Object.freeze(values) as Imported<I>;
  }

  async dispatch(
    method: string,
    params: unknown,
    signal: AbortSignal,
  ): Promise<unknown> {
    switch (method) {
      case wire.methods.hello: {
        this.#expect("new");
        exactFields(params, ["version", "maxFrameBytes"]);
        if (
          params.version !== wire.version ||
          !Number.isSafeInteger(params.maxFrameBytes) ||
          (params.maxFrameBytes as number) < 1
        )
          throw new ProcessError(
            wire.errors.contract,
            "incompatible process profile",
          );
        const maxFrameBytes = Math.min(
          wire.maxFrameBytes,
          params.maxFrameBytes as number,
        );
        this.peer.limit(maxFrameBytes);
        this.#phase = "described";
        return {
          version: wire.version,
          unit: this.unit.id,
          maxFrameBytes,
          imports: Object.fromEntries(
            Object.entries(this.unit.imports).map(([k, t]) => [
              k,
              ("contract" in t ? t.contract : t).identity,
            ]),
          ),
          exports: Object.fromEntries(
            Object.entries(this.unit.exports).map(([k, t]) => [k, t.identity]),
          ),
        };
      }
      case wire.methods.prepare: {
        this.#expect("described");
        exactFields(params, ["imports"]);
        const imports = this.#imports(params.imports);
        this.#phase = "preparing";
        try {
          const handlers = await this.lifetime.run(() =>
            this.unit.setup({
              imports,
              lifetime: this.lifetime,
              onActivate: (start) => {
                this.#expect("preparing");
                if (this.#activate)
                  throw new TypeError("activation already registered");
                this.#activate = start;
              },
            }),
          );
          exactFields(handlers, Object.keys(this.unit.exports));
          const table: Record<string, Record<string, Handler>> = Object.create(
            null,
          );
          for (const [name, contract] of Object.entries(this.unit.exports)) {
            const members = handlers[name];
            exactFields(
              members,
              contract.descriptor.methods.map((m) => m.name),
            );
            if (
              Object.values(members).some(
                (handler) => typeof handler !== "function",
              )
            )
              throw new TypeError("required member is not callable");
            table[name] = Object.freeze({ ...members }) as Record<
              string,
              Handler
            >;
          }
          signal.throwIfAborted();
          this.#expect("preparing");
          this.#exports = Object.freeze(table);
          this.#phase = "prepared";
          return true;
        } catch (error) {
          this.#phase = "closed";
          await this.lifetime.close();
          throw error;
        }
      }
      case wire.methods.activate: {
        this.#expect("prepared");
        exactFields(params, []);
        this.#phase = "active";
        try {
          await this.lifetime.run(() =>
            this.#activate?.({
              start: (job) => {
                this.#expect("active");
                void this.lifetime.run(job).catch((error) => {
                  if (!this.lifetime.signal.aborted) this.peer.stop(error);
                });
              },
            }),
          );
          signal.throwIfAborted();
          return true;
        } catch (error) {
          this.#phase = "closed";
          await this.lifetime.close();
          throw error;
        }
      }
      case wire.methods.close: {
        exactFields(params, []);
        this.#phase = "closed";
        const outcome = await this.lifetime.close();
        if (outcome.status !== "released")
          throw new ProcessError(
            wire.errors.unknown,
            "resource release unconfirmed",
          );
        return true;
      }
      case wire.methods.chunk: {
        this.#expect("active");
        exactFields(params, ["stream", "value"]);
        const receive =
          typeof params.stream === "string"
            ? this.#streams.get(params.stream)
            : undefined;
        if (!receive) throw new TypeError("unknown stream handle");
        await receive(params.value, signal);
        return true;
      }
      case wire.methods.call:
        return this.#call(params, signal);
      default:
        throw new ProcessError(wire.errors.invalid, "unknown process method");
    }
  }

  #boundCall(params: unknown) {
    this.#expect("active");
    try {
      exactFields(
        params,
        ["handle", "member", "input"],
        ["stream", "commandID"],
      );
      if (
        !validToken(params.handle) ||
        !validToken(params.member) ||
        !Object.hasOwn(this.#exports, params.handle)
      )
        throw new TypeError("unknown bound member");
      const service = this.unit.exports[params.handle]!;
      const operation = service.method(params.member);
      const handler = this.#exports[params.handle]![params.member]!;
      operation.validateInvocation(params.input, params.commandID);
      if (
        operation.validateChunk
          ? !validToken(params.stream)
          : params.stream !== undefined
      )
        throw new TypeError("stream mode mismatch");
      return {
        operation,
        handler,
        input: params.input,
        stream: params.stream,
        commandID: params.commandID as string | undefined,
      };
    } catch (cause) {
      throw new ProcessError(
        wire.errors.contract,
        "service contract mismatch",
        { cause },
      );
    }
  }

  async #call(params: unknown, signal: AbortSignal): Promise<unknown> {
    const { operation, handler, input, stream, commandID } =
      this.#boundCall(params);
    return this.lifetime.run(async (lifetimeSignal) => {
      const active = AbortSignal.any([lifetimeSignal, signal]);
      const call = operation.validateChunk ? new StreamCall(active) : undefined;
      const invoke = async (running: AbortSignal) => {
        running.throwIfAborted();
        try {
          const result = await handler(input, {
            signal: running,
            commandID,
            emit: (chunk) => {
              if (!call || !operation.validateChunk)
                return Promise.reject(
                  new TypeError("method does not support chunk delivery"),
                );
              return call.deliver(running, async (delivery) => {
                operation.validateChunk!(chunk);
                if (
                  (await this.peer.request(
                    wire.methods.chunk,
                    { stream, value: chunk },
                    delivery,
                  )) !== true
                )
                  throw new Error("chunk not acknowledged");
              });
            },
          });
          operation.validateOutput(result);
          return result;
        } catch (cause) {
          throw ProcessError.afterDispatch(cause);
        }
      };
      return call ? call.run(invoke) : invoke(active);
    });
  }
}

export async function serve<I extends Imports, E extends Exports>(
  unit: ProcessUnit<I, E>,
  options: { input?: Readable; output?: Writable } = {},
): Promise<void> {
  const peer = new Peer(
    options.input ?? process.stdin,
    options.output ?? process.stdout,
  );
  const instance = new Instance(defineProcessUnit(unit), peer);
  peer.signal.addEventListener("abort", () => instance.lifetime.beginClose(), {
    once: true,
  });
  try {
    await peer.run((name, params, signal) =>
      instance.dispatch(name, params, signal),
    );
  } finally {
    await instance.lifetime.close();
  }
}
