import {
  type ContractValue,
  type Event,
  type Optional,
  type Point,
  type Service,
} from "./contracts.js";
import { identity } from "./identity.js";
import { type Awaitable, type Lifetime } from "./lifetime.js";

export type Imports = Readonly<
  Record<string, Service<unknown> | Optional<Service<unknown>>>
>;
export type Exports = Readonly<
  Record<string, Service<unknown> | Point<unknown, unknown> | Event<unknown>>
>;
export type Imported<R extends Imports> = {
  readonly [K in keyof R]: R[K] extends Optional<infer T>
    ? ContractValue<T> | undefined
    : ContractValue<R[K]>;
};
export type Exported<P extends Exports> = {
  readonly [K in keyof P]: P[K] extends Point<infer M, infer V>
    ? Contribution<M, V>
    : ContractValue<P[K]>;
};

export interface Contribution<M, V> {
  readonly metadata: M;
  readonly value: V;
}

export function contribution<M, V>(metadata: M, value: V): Contribution<M, V> {
  return Object.freeze({ metadata, value });
}

export interface Setup<R extends Imports> {
  readonly imports: Imported<R>;
  readonly configuration: unknown;
  readonly lifetime: Lifetime;
}

export interface Unit<
  R extends Imports = Imports,
  P extends Exports = Exports,
> {
  readonly id: string;
  readonly imports: R;
  readonly exports: P;
  setup(context: Setup<R>): Awaitable<Exported<P>>;
}

export function defineUnit<const R extends Imports, const P extends Exports>(
  definition: Unit<R, P>,
): Unit<R, P> {
  identity(definition.id, "unit id");
  if (typeof definition.setup !== "function")
    throw new TypeError("unit setup must be a function");
  for (const name of [
    ...Object.keys(definition.imports),
    ...Object.keys(definition.exports),
  ])
    identity(name, "unit slot");
  return Object.freeze({
    ...definition,
    imports: Object.freeze({ ...definition.imports }),
    exports: Object.freeze({ ...definition.exports }),
  });
}
