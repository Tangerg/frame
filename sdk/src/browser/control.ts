import type {
  Authority,
  ControlInputs,
  ControlOutputs,
} from "../protocol.generated.js";
import { FrameError } from "../errors.js";

/** The application supplies an authenticated transport to its single Go Host. */
export type HostControl = {
  [K in keyof ControlInputs]: ControlInputs[K] extends Record<string, never>
    ? () => Promise<ControlOutputs[K]>
    : (input: ControlInputs[K]) => Promise<ControlOutputs[K]>;
};

// A refusal excludes effects from this call, not from earlier uncertain dispatches.
export function isControlRefusal(error: unknown): boolean {
  return (
    error instanceof FrameError &&
    [
      "permission_denied",
      "invalid_plan",
      "contract_mismatch",
      "stale_binding",
      "capacity",
      "closed",
    ].includes(error.code)
  );
}

export function sameAuthority(left: Authority, right: Authority): boolean {
  return left.id === right.id && left.epoch === right.epoch;
}
