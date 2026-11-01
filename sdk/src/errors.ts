export type ErrorCode =
  | "closed"
  | "capacity"
  | "contract_mismatch"
  | "invalid_plan"
  | "unavailable"
  | "stale_binding"
  | "permission_denied"
  | "outcome_unknown"
  | "close_unconfirmed"
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

export function nonempty(value: string, label: string): string {
  if (typeof value !== "string" || !value.trim() || value.length > 1024) {
    throw new TypeError(`${label} must be a nonempty bounded string`);
  }
  return value;
}
