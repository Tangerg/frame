export { FrameError, type ErrorCode } from "./errors.js";
export {
  Lifetime,
  type LifetimeLimits,
  type Awaitable,
  type Failure,
  type Release,
  type CloseOutcome,
  type Exit,
  type Cleanup,
} from "./lifetime.js";
export {
  acquire,
  withResource,
  type Acquired,
  type AcquiredResource,
} from "./use.js";
export {
  Service,
  Point,
  Event,
  optional,
  sameContract,
  type Contract,
  type ContractValue,
  type Optional,
} from "./contracts.js";
export {
  defineUnit,
  type Unit,
  type Setup,
  type Imports,
  type Exports,
  type Imported,
  type Exported,
  contribution,
  type Contribution,
} from "./unit.js";
