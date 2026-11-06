import { CONTROL_ERROR_CODES } from "./protocol.generated.js";

const errorCodes = Object.freeze([
  ...CONTROL_ERROR_CODES,
  "close_failed",
  "resync_required",
  "reload_required",
  "reentrant_operation",
] as const);

export type ErrorCode = (typeof errorCodes)[number];

export function isErrorCode(value: unknown): value is ErrorCode {
  return errorCodes.some((code) => code === value);
}

export class FrameError extends Error {
  readonly code: ErrorCode;

  constructor(code: ErrorCode, message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = "FrameError";
    this.code = code;
  }
}
