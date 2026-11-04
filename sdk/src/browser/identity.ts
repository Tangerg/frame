import { sameContract } from "../contracts.js";
import type {
  Authority,
  Execution,
  ObjectRef,
  Realm,
} from "../protocol.generated.js";

export function sameAuthority(left: Authority, right: Authority): boolean {
  return left.id === right.id && left.epoch === right.epoch;
}

export function sameRealm(left: Realm, right: Realm): boolean {
  return left.id === right.id && left.epoch === right.epoch;
}

export function sameExecution(left: Execution, right: Execution): boolean {
  return left.id === right.id && left.generation === right.generation;
}

export function sameObject(left: ObjectRef, right: ObjectRef): boolean {
  return (
    left.objectId === right.objectId &&
    left.export.instanceId === right.export.instanceId &&
    left.export.name === right.export.name &&
    sameContract(left.export.contract, right.export.contract)
  );
}
