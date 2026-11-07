/// <reference types="node" preserve="true" />
import { randomUUID } from "node:crypto";
import type { Readable, Writable } from "node:stream";
import { Lifetime, type Awaitable } from "../index.js";
import { DIGEST_PATTERN } from "../protocol.generated.js";
import {
  Capability,
  admitShape,
  exactFields,
  validToken,
  type ServiceContract,
  type Handlers,
  type Client,
  type RequestOptions,
  type CallContext,
} from "./contract.js";
import { Peer, ProcessError } from "./peer.js";
import shapes from "./shapes.generated.json" with { type: "json" };
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
  const id = unit.id;
  const sourceImports = { ...unit.imports };
  const exports = Object.freeze({ ...unit.exports });
  const setup = unit.setup;
  if (!validToken(id) || typeof setup !== "function")
    throw new TypeError("invalid process unit");
  const imports: Record<string, Import> = Object.create(null);
  for (const [name, token] of Object.entries(sourceImports)) {
    if (!validToken(name)) throw new TypeError("invalid import declaration");
    if (token instanceof Capability) {
      imports[name] = token;
      continue;
    }
    if (!("contract" in token))
      throw new TypeError("invalid import declaration");
    const captured = Object.freeze({
      contract: token.contract,
      cardinality: token.cardinality,
    });
    if (
      !(captured.contract instanceof Capability) ||
      (captured.cardinality !== "many" && captured.cardinality !== "optional")
    )
      throw new TypeError("invalid import declaration");
    imports[name] = captured;
  }
  for (const [name, contract] of Object.entries(exports)) {
    if (!validToken(name) || !(contract instanceof Capability))
      throw new TypeError("invalid export declaration");
  }
  const capturedSetup: ProcessUnit<I, E>["setup"] = (context) =>
    Reflect.apply(setup, unit, [context]);
  return Object.freeze({
    id,
    imports: Object.freeze(imports) as I,
    exports,
    setup: capturedSetup,
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

const digestPattern = new RegExp(DIGEST_PATTERN);

function digest(value: unknown): value is string {
  return typeof value === "string" && digestPattern.test(value);
}

function checkPortableRef(
  value: unknown,
): asserts value is Record<string, unknown> {
  admitShape(value, shapes.contract);
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
              const wait = options.signal;
              const onChunk = options.onChunk;
              const captured = method.admitInvocation(input, commandID);
              const signal =
                wait === undefined
                  ? this.lifetime.signal
                  : AbortSignal.any([this.lifetime.signal, wait]);
              if (
                method.admitChunk
                  ? typeof onChunk !== "function"
                  : onChunk !== undefined
              )
                throw new TypeError(
                  "stream receiver differs from the declared method",
                );
              const stream = onChunk ? randomUUID() : undefined;
              const call = stream ? new StreamCall(signal) : undefined;
              if (stream && call && onChunk) {
                this.#streams.set(stream, (chunk, wait) =>
                  call.deliver(wait, async () => {
                    await Reflect.apply(onChunk, options, [
                      method.admitChunk!(chunk),
                    ]);
                  }),
                );
              }
              const invoke = async (active: AbortSignal) => {
                const result = await this.peer.request(
                  wire.methods.call,
                  {
                    handle,
                    member: descriptor.name,
                    input: captured,
                    stream,
                    commandID,
                  },
                  active,
                );
                try {
                  return method.admitOutput(result);
                } catch (cause) {
                  throw ProcessError.afterDispatch(cause);
                }
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
        admitShape(binding, shapes.grant);
        admitShape(binding.source, shapes.source);
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
      case wire.methods.hello:
        return this.#hello(params);
      case wire.methods.prepare:
        return this.#prepare(params, signal);
      case wire.methods.activate:
        return this.#activateUnit(params, signal);
      case wire.methods.close:
        return this.#close(params);
      case wire.methods.chunk:
        return this.#chunk(params, signal);
      case wire.methods.call:
        return this.#call(params, signal);
      default:
        throw new ProcessError(wire.errors.invalid, "unknown process method");
    }
  }

  #hello(params: unknown) {
    this.#expect("new");
    admitShape(params, shapes.hello);
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

  async #prepare(params: unknown, signal: AbortSignal) {
    this.#expect("described");
    admitShape(params, shapes.preparation);
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
      if (
        handlers === null ||
        typeof handlers !== "object" ||
        Array.isArray(handlers)
      )
        throw new TypeError("unit implementation must be an object");
      const captured = { ...handlers };
      exactFields(captured, Object.keys(this.unit.exports));
      const table: Record<string, Record<string, Handler>> = Object.create(
        null,
      );
      for (const [name, contract] of Object.entries(this.unit.exports)) {
        table[name] = contract.implement(captured[name]!) as Record<
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

  async #activateUnit(params: unknown, signal: AbortSignal) {
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

  async #close(params: unknown) {
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

  async #chunk(params: unknown, signal: AbortSignal) {
    this.#expect("active");
    admitShape(params, shapes.chunk);
    const receive =
      typeof params.stream === "string"
        ? this.#streams.get(params.stream)
        : undefined;
    if (!receive) throw new TypeError("unknown stream handle");
    await receive(params.value, signal);
    return true;
  }

  #boundCall(params: unknown) {
    this.#expect("active");
    try {
      admitShape(params, shapes.call);
      if (
        !validToken(params.handle) ||
        !validToken(params.member) ||
        !Object.hasOwn(this.#exports, params.handle)
      )
        throw new TypeError("unknown bound member");
      const service = this.unit.exports[params.handle]!;
      const operation = service.method(params.member);
      const handler = this.#exports[params.handle]![params.member]!;
      const input = operation.admitInvocation(params.input, params.commandID);
      if (
        operation.admitChunk
          ? !validToken(params.stream)
          : params.stream !== undefined
      )
        throw new TypeError("stream mode mismatch");
      return {
        operation,
        handler,
        input,
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
      const call = operation.admitChunk ? new StreamCall(active) : undefined;
      const invoke = async (running: AbortSignal) => {
        running.throwIfAborted();
        try {
          const result = await handler(input, {
            signal: running,
            commandID,
            emit: (chunk) => {
              if (!call || !operation.admitChunk)
                return Promise.reject(
                  new TypeError("method does not support chunk delivery"),
                );
              return call.deliver(running, async (delivery) => {
                const captured = operation.admitChunk!(chunk);
                if (
                  (await this.peer.request(
                    wire.methods.chunk,
                    { stream, value: captured },
                    delivery,
                  )) !== true
                )
                  throw new Error("chunk not acknowledged");
              });
            },
          });
          return operation.admitOutput(result);
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
