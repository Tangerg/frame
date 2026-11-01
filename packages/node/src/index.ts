/// <reference types="node" preserve="true" />
import { randomUUID } from "node:crypto";
import type { Readable, Writable } from "node:stream";
import { Lifetime, type Awaitable } from "@frame/sdk";
import {
  Capability,
  exactFields,
  validToken,
  type ServiceContract,
  type Handlers,
  type Client,
  type CallOptions,
  type CallContext,
} from "./contract.js";
import { Peer, ProcessError } from "./peer.js";
import wire from "./wire.json" with { type: "json" };

export {
  Capability,
  type CapabilityDescriptor,
  type MethodDescriptor,
} from "./contract.js";
export type { Client, CallOptions, Handlers } from "./contract.js";
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
  readonly #streams = new Map<string, (chunk: unknown) => Promise<void>>();
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
            async (input: unknown, options: CallOptions = {}) => {
              this.#expect("active");
              method.validateInput(input);
              const signal = options.signal
                ? AbortSignal.any([this.lifetime.signal, options.signal])
                : this.lifetime.signal;
              signal.throwIfAborted();
              if (Boolean(method.validateChunk) !== Boolean(options.onChunk))
                throw new TypeError(
                  "stream receiver differs from the declared method",
                );
              const stream = options.onChunk ? randomUUID() : undefined;
              if (stream) {
                let busy = false;
                this.#streams.set(stream, async (chunk: unknown) => {
                  signal.throwIfAborted();
                  if (busy)
                    throw new ProcessError(
                      wire.errors.invalid,
                      "concurrent stream delivery",
                    );
                  busy = true;
                  try {
                    method.validateChunk!(chunk);
                    await options.onChunk!(chunk);
                  } finally {
                    busy = false;
                  }
                });
              }
              try {
                const result = await this.peer.request(
                  wire.methods.call,
                  { handle, member: descriptor.name, input, stream },
                  signal,
                );
                method.validateOutput(result);
                return result;
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
        await receive(params.value);
        return true;
      }
      case wire.methods.call:
        return this.#call(params, signal);
      default:
        throw new ProcessError(wire.errors.invalid, "unknown process method");
    }
  }

  async #call(params: unknown, signal: AbortSignal): Promise<unknown> {
    this.#expect("active");
    exactFields(params, ["handle", "member", "input"], ["stream"]);
    if (
      !validToken(params.handle) ||
      !validToken(params.member) ||
      !Object.hasOwn(this.#exports, params.handle)
    )
      throw new ProcessError(wire.errors.contract, "unknown bound member");
    const service = this.unit.exports[params.handle]!;
    const operation = service.method(params.member);
    const handler = this.#exports[params.handle]![params.member]!;
    operation.validateInput(params.input);
    if (
      operation.validateChunk
        ? !validToken(params.stream)
        : params.stream !== undefined
    )
      throw new TypeError("stream mode mismatch");
    return this.lifetime.run(async (lifetimeSignal) => {
      const active = AbortSignal.any([lifetimeSignal, signal]);
      let open = true,
        busy = false;
      try {
        active.throwIfAborted();
        const result = await handler(params.input, {
          signal: active,
          emit: async (value) => {
            if (!open || busy || !operation.validateChunk)
              throw new TypeError("chunk delivery not available");
            active.throwIfAborted();
            busy = true;
            try {
              operation.validateChunk(value);
              if (
                (await this.peer.request(
                  wire.methods.chunk,
                  { stream: params.stream, value },
                  active,
                )) !== true
              )
                throw new Error("chunk not acknowledged");
            } finally {
              busy = false;
            }
          },
        });
        if (busy) throw new Error("unjoined chunk delivery");
        operation.validateOutput(result);
        return result;
      } catch (error) {
        if (error instanceof ProcessError) throw error;
        throw new ProcessError(
          wire.errors.unknown,
          "operation outcome unconfirmed",
        );
      } finally {
        open = false;
      }
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
