import { type Awaitable, type Lifetime } from "../index.js";
import { validName } from "../identity.js";
import {
  Capability,
  type Client,
  type Handlers,
  type ServiceContract,
} from "./contract.js";

export const optional = <C extends ServiceContract>(contract: C) =>
  Object.freeze({ contract, cardinality: "optional" as const });
export const collection = <C extends ServiceContract>(contract: C) =>
  Object.freeze({ contract, cardinality: "many" as const });
export type Import =
  | ServiceContract
  | {
      readonly contract: ServiceContract;
      readonly cardinality: "optional" | "many";
    };
export type Imports = Readonly<Record<string, Import>>;
export type Exports = Readonly<Record<string, ServiceContract>>;
export type Imported<I extends Imports> = {
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
  if (!validName(id) || typeof setup !== "function")
    throw new TypeError("invalid process unit");
  const imports: Record<string, Import> = Object.create(null);
  for (const [name, token] of Object.entries(sourceImports)) {
    if (!validName(name)) throw new TypeError("invalid import declaration");
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
    if (!validName(name) || !(contract instanceof Capability))
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
