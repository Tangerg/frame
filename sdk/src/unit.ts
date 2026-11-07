import {
  type ContractValue,
  Event,
  type Optional,
  Point,
  Service,
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
  const id = definition.id;
  const declaredImports = { ...definition.imports };
  const exports = Object.freeze({ ...definition.exports });
  const setup = definition.setup;
  identity(id, "unit id");
  if (typeof setup !== "function")
    throw new TypeError("unit setup must be a function");
  const imports: Record<string, Service<unknown> | Optional<Service<unknown>>> =
    Object.create(null);
  for (const [name, declared] of Object.entries(declaredImports)) {
    identity(name, "unit slot");
    if (declared instanceof Service) {
      imports[name] = declared;
      continue;
    }
    const contract = declared.contract;
    const optional = declared.optional;
    if (!(contract instanceof Service) || optional !== true)
      throw new TypeError("unit import must declare a service");
    imports[name] = Object.freeze({ contract, optional });
  }
  for (const [name, contract] of Object.entries(exports)) {
    identity(name, "unit slot");
    if (
      !(contract instanceof Service) &&
      !(contract instanceof Point) &&
      !(contract instanceof Event)
    )
      throw new TypeError("unit export must declare a native contract");
  }
  return Object.freeze({
    id,
    imports: Object.freeze(imports) as R,
    exports,
    setup: setup.bind(definition),
  });
}
