import type { ControlInputs, ControlOutputs } from "../protocol.generated.js";
import { FrameError } from "../errors.js";

/** The application supplies an authenticated transport to its single Go Host. */
export type HostControl = {
  [K in keyof ControlInputs]: ControlInputs[K] extends Record<string, never>
    ? () => Promise<ControlOutputs[K]>
    : (input: ControlInputs[K]) => Promise<ControlOutputs[K]>;
};

// A refusal excludes effects from this call, not from earlier uncertain dispatches.
export function isControlRefusal(error: unknown): boolean {
  try {
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
  } catch {
    // Failed inspection cannot prove that a mutation was refused before dispatch.
    return false;
  }
}
