import type { CONTROL_ERROR_CODES } from "./protocol.generated.js";

export type ErrorCode =
  | (typeof CONTROL_ERROR_CODES)[number]
  | "close_failed"
  | "resync_required"
  | "reload_required"
  | "reentrant_operation";

export class FrameError extends Error {
  readonly code: ErrorCode;

  constructor(code: ErrorCode, message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = "FrameError";
    this.code = code;
  }
}
