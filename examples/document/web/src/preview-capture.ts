import { FrameError } from "@frame/sdk";
import type { HostControl } from "@frame/sdk/browser";
import type { Snapshot, SnapshotRequest } from "@frame/sdk/protocol";

export class PreviewSnapshots {
  readonly #point: string;
  #request?: SnapshotRequest;
  #snapshot?: Snapshot;
  #capturing?: Promise<Snapshot>;
  #closing?: Promise<void>;

  constructor(point: string) {
    this.#point = point;
  }

  capture(control: Pick<HostControl, "snapshot">): Promise<Snapshot> {
    if (this.#closing)
      return Promise.reject(
        new FrameError("closed", "preview snapshot is closing"),
      );
    if (this.#snapshot) return Promise.resolve(this.#snapshot);
    if (this.#capturing) return this.#capturing;
    const request = (this.#request ??= Object.freeze({
      operationId: crypto.randomUUID(),
      points: Object.freeze([this.#point]),
    }));
    // A later refusal cannot disprove acceptance of an earlier dispatch.
    this.#capturing = Promise.resolve()
      .then(() => control.snapshot(request))
      .then((snapshot) => {
        if (snapshot.id !== request.operationId)
          throw new FrameError(
            "outcome_unknown",
            "preview capture returned another snapshot identity",
          );
        this.#snapshot = snapshot;
        return snapshot;
      })
      .finally(() => {
        this.#capturing = undefined;
      });
    return this.#capturing;
  }

  consumed(snapshotId: string): void {
    if (this.#request?.operationId !== snapshotId || this.#closing)
      throw new FrameError(
        "invalid_plan",
        "preview acquisition changed its original snapshot",
      );
    this.#request = undefined;
    this.#snapshot = undefined;
  }

  close(control: Pick<HostControl, "closeSnapshot">): Promise<void> {
    if (this.#closing) return this.#closing;
    const request = this.#request;
    if (!request) return Promise.resolve();
    this.#closing = Promise.resolve(this.#capturing)
      .catch(() => {})
      .then(() => control.closeSnapshot(request.operationId))
      .then(() => {
        this.#request = undefined;
        this.#snapshot = undefined;
      })
      .finally(() => {
        this.#closing = undefined;
      });
    return this.#closing;
  }
}
