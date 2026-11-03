import {
  ContractModeValues,
  DIGEST_PATTERN,
  type ContractRef,
} from "./protocol.generated.js";

const digestPattern = new RegExp(DIGEST_PATTERN);
import { FrameError } from "./errors.js";
import { identity } from "./identity.js";

export function sameContract(left: ContractRef, right: ContractRef): boolean {
  return (
    left.name === right.name &&
    left.version === right.version &&
    left.digest === right.digest &&
    left.kind === right.kind &&
    left.mode === right.mode
  );
}

function checked(ref: ContractRef, kind: ContractRef["kind"]): ContractRef {
  identity(ref.name, "contract name");
  identity(ref.version, "contract version");
  if (
    !digestPattern.test(ref.digest) ||
    ref.kind !== kind ||
    !ContractModeValues.includes(ref.mode)
  ) {
    throw new FrameError(
      "contract_mismatch",
      "invalid published contract reference",
    );
  }
  return Object.freeze({ ...ref });
}

export class Service<T> {
  declare private readonly valueType: T;
  readonly ref: ContractRef;

  constructor(ref: ContractRef) {
    this.ref = checked(ref, "service");
    Object.freeze(this);
  }
}

export class Point<M, V> {
  declare private readonly metadataType: M;
  declare private readonly valueType: V;
  readonly ref: ContractRef;

  constructor(ref: ContractRef) {
    this.ref = checked(ref, "point");
    Object.freeze(this);
  }
}

export class Event<T> {
  declare private readonly valueType: T;
  readonly ref: ContractRef;

  constructor(ref: ContractRef) {
    this.ref = checked(ref, "event");
    Object.freeze(this);
  }
}

export type Contract<T> = Service<T> | Point<unknown, T> | Event<T>;
export type ContractValue<T> =
  T extends Service<infer V>
    ? V
    : T extends Point<unknown, infer V>
      ? V
      : T extends Event<infer V>
        ? V
        : never;

export interface Optional<T> {
  readonly contract: T;
  readonly optional: true;
}

export function optional<T extends Service<unknown>>(contract: T): Optional<T> {
  return Object.freeze({ contract, optional: true });
}
