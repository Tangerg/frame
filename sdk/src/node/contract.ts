import type { Awaitable } from "../index.js";
import type { JSONNumber } from "../json.js";
import { createHash } from "node:crypto";
import { canonicalJSON, freezeJSON } from "../json.js";
import { compileSchema, SchemaMismatch } from "./schema.js";
import profile from "./profile.generated.json" with { type: "json" };
import { captureDescriptor, exceeded, PORTABLE_LIMITS } from "./limits.js";

export const PROTOCOL = profile.protocol;
const maximumUTF16CodeUnitsPerCodePoint = 2;

export interface MethodDescriptor {
  name: string;
  kind: "query" | "command";
  target?: string;
  description: string;
  input: unknown;
  output: unknown;
  stream?: unknown;
}
export interface CapabilityDescriptor {
  protocol: string;
  reference: string;
  methods: readonly MethodDescriptor[];
}
export function digestJSON(value: unknown): string {
  return createHash("sha256").update(canonicalJSON(value)).digest("hex");
}
function boundedUTF8(value: unknown, maximumBytes: number): value is string {
  return (
    typeof value === "string" &&
    value.length > 0 &&
    Buffer.byteLength(value) <= maximumBytes &&
    !/\p{Surrogate}/u.test(value)
  );
}
export function validToken(value: unknown): value is string {
  return (
    boundedUTF8(value, PORTABLE_LIMITS.maxTokenBytes) &&
    !/[\p{White_Space}\p{Cc}]/u.test(value)
  );
}
export function compareTokens(left: string, right: string): number {
  // Go orders protocol tokens by UTF-8 bytes, independently of JSON key order.
  return Buffer.compare(Buffer.from(left), Buffer.from(right));
}
/** The member set of a Go-owned wire type, projected by its owner. */
export interface Shape {
  readonly required: readonly string[];
  readonly optional: readonly string[];
}

/** Admits an object carrying exactly the shape's members. */
export function admitShape(
  value: unknown,
  shape: Shape,
): asserts value is Record<string, unknown> {
  exactFields(value, shape.required, shape.optional);
}

export function exactFields(
  value: unknown,
  required: readonly string[],
  optional: readonly string[] = [],
): asserts value is Record<string, unknown> {
  if (
    value === null ||
    typeof value !== "object" ||
    Array.isArray(value) ||
    required.some((key) => !Object.hasOwn(value, key)) ||
    Object.keys(value).some(
      (key) => !required.includes(key) && !optional.includes(key),
    )
  )
    throw new Error("plugin: invalid protocol members");
}

export class Method {
  readonly descriptor: MethodDescriptor;
  readonly identity: string;
  readonly validateInput: (value: unknown) => void;
  readonly validateOutput: (value: unknown) => void;
  readonly validateChunk?: (value: unknown) => void;

  constructor(source: MethodDescriptor) {
    const descriptor = freezeJSON(captureDescriptor(source));
    admitShape(descriptor, profile.descriptors.method);
    if (
      !validToken(descriptor.name) ||
      (descriptor.target !== undefined && !validToken(descriptor.target)) ||
      !profile.descriptors.methodKinds.includes(descriptor.kind) ||
      typeof descriptor.description !== "string"
    )
      throw new Error("plugin: invalid method contract");
    this.descriptor = descriptor;
    this.validateInput = compileSchema(descriptor.input);
    let admitsNull = true;
    try {
      this.validateInput(null);
    } catch (cause) {
      if (!(cause instanceof SchemaMismatch)) throw cause;
      admitsNull = false;
    }
    if (admitsNull) throw new Error("plugin: method input must exclude null");
    this.validateOutput = compileSchema(descriptor.output);
    if (Object.hasOwn(descriptor, "stream"))
      this.validateChunk = compileSchema(descriptor.stream);
    this.identity = digestJSON({ ...descriptor, description: "" });
    Object.freeze(this);
  }

  validateInvocation(input: unknown, commandID: unknown): void {
    if (this.descriptor.kind === "command") {
      if (
        typeof commandID !== "string" ||
        commandID.length === 0 ||
        commandID.length >
          maximumUTF16CodeUnitsPerCodePoint *
            PORTABLE_LIMITS.maxCommandIDCharacters ||
        /\p{Surrogate}/u.test(commandID) ||
        [...commandID].length > PORTABLE_LIMITS.maxCommandIDCharacters
      )
        throw new TypeError("plugin: bounded command identity required");
    } else if (commandID !== undefined) {
      throw new TypeError("plugin: query must not carry a command identity");
    }
    this.validateInput(input);
  }
}

type RequiredKeys<S> = S extends {
  required: readonly (infer K extends string)[];
}
  ? K
  : never;

type SchemaValue<S> = S extends { const: infer T }
  ? T
  : S extends { enum: readonly (infer T)[] }
    ? T
    : S extends { type: "string" }
      ? string
      : S extends { type: "boolean" }
        ? boolean
        : S extends { type: "integer" | "number" }
          ? JSONNumber
          : S extends { type: "null" }
            ? null
            : S extends { type: "array"; items: infer T }
              ? SchemaValue<T>[]
              : S extends {
                    type: "object";
                    properties: infer P;
                  }
                ? {
                    [
                      N in keyof P as N extends RequiredKeys<S> ? N : never
                    ]: SchemaValue<P[N]>;
                  } & {
                    [
                      N in keyof P as N extends RequiredKeys<S> ? never : N
                    ]?: SchemaValue<P[N]>;
                  }
                : unknown;
export interface ServiceContract {
  readonly descriptor: CapabilityDescriptor;
  readonly identity: string;
  method(name: string): Method;
}
type Described<C extends ServiceContract> = C["descriptor"]["methods"][number];
export interface RequestOptions<C = unknown> {
  signal?: AbortSignal;
  onChunk?: (value: C) => Awaitable<void>;
  commandID?: string;
}
export interface CallContext<
  C,
  K extends MethodDescriptor["kind"] = MethodDescriptor["kind"],
> {
  readonly signal: AbortSignal;
  readonly commandID: K extends "command" ? string : undefined;
  emit(value: C): Promise<void>;
}
type Chunk<M extends MethodDescriptor> = M extends { stream: infer S }
  ? SchemaValue<S>
  : never;
type RequestArguments<M extends MethodDescriptor> = M["kind"] extends "command"
  ? [options: RequestOptions<Chunk<M>> & { commandID: string }]
  : M["kind"] extends "query"
    ? [options?: RequestOptions<Chunk<M>> & { commandID?: never }]
    : [options?: RequestOptions<Chunk<M>>];
export type Client<C extends ServiceContract> = {
  readonly [M in Described<C> as M["name"]]: (
    input: SchemaValue<M["input"]>,
    ...request: RequestArguments<M>
  ) => Promise<SchemaValue<M["output"]>>;
};
export type Handlers<C extends ServiceContract> = {
  [M in Described<C> as M["name"]]: (
    input: SchemaValue<M["input"]>,
    context: CallContext<Chunk<M>, M["kind"]>,
  ) => Awaitable<SchemaValue<M["output"]>>;
};

export class Capability<
  const D extends CapabilityDescriptor = CapabilityDescriptor,
> {
  readonly descriptor: D;
  readonly identity: string;
  readonly #methods: ReadonlyMap<string, Method>;

  constructor(source: D) {
    const descriptor = freezeJSON(captureDescriptor(source));
    admitShape(descriptor, profile.descriptors.capability);
    if (
      descriptor.protocol !== PROTOCOL ||
      !validToken(descriptor.reference) ||
      !Array.isArray(descriptor.methods) ||
      descriptor.methods.length === 0
    )
      throw new Error("plugin: invalid capability contract");
    if (descriptor.methods.length > PORTABLE_LIMITS.maxMethods)
      exceeded("capability methods", PORTABLE_LIMITS.maxMethods);
    const methods = descriptor.methods
      .map((value) => new Method(value))
      .sort((a, b) => compareTokens(a.descriptor.name, b.descriptor.name));
    this.#methods = new Map(
      methods.map((method) => [method.descriptor.name, method]),
    );
    if (this.#methods.size !== methods.length)
      throw new Error("plugin: duplicate capability method");
    this.descriptor = descriptor;
    this.identity = digestJSON({
      ...this.descriptor,
      methods: methods.map((method) => ({
        ...method.descriptor,
        description: "",
      })),
    });
    Object.freeze(this);
  }
  implement(handlers: Handlers<Capability<D>>): Handlers<Capability<D>> {
    exactFields(
      handlers,
      this.descriptor.methods.map((method) => method.name),
    );
    Object.freeze(handlers);
    return handlers;
  }

  method(name: string): Method {
    const method = this.#methods.get(name);
    if (!method) throw new Error("plugin: capability method is not published");
    return method;
  }
}
