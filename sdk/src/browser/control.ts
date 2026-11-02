import type {
  Authority,
  ControlInputs,
  ControlOutputs,
} from "../protocol.generated.js";

/** The application supplies an authenticated transport to its single Go Host. */
export type HostControl = {
  [K in keyof ControlInputs]: ControlInputs[K] extends Record<string, never>
    ? () => Promise<ControlOutputs[K]>
    : (input: ControlInputs[K]) => Promise<ControlOutputs[K]>;
};

export function sameAuthority(left: Authority, right: Authority): boolean {
  return left.id === right.id && left.epoch === right.epoch;
}
