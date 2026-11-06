import type { HostControl } from "@frame/sdk/browser";
import type { Snapshot, SnapshotRequest } from "@frame/sdk/protocol";

export class PreviewSnapshots {
  readonly #point: string;
  #request?: SnapshotRequest;
  #capturing?: Promise<Snapshot>;

  constructor(point: string) {
    this.#point = point;
  }

  capture(control: Pick<HostControl, "snapshot">): Promise<Snapshot> {
    if (this.#capturing) return this.#capturing;
    const request = (this.#request ??= Object.freeze({
      operationId: crypto.randomUUID(),
      points: Object.freeze([this.#point]),
    }));
    // A later refusal cannot disprove acceptance of an earlier dispatch.
    this.#capturing = Promise.resolve()
      .then(() => control.snapshot(request))
      .then((snapshot) => {
        this.#request = undefined;
        return snapshot;
      })
      .finally(() => {
        this.#capturing = undefined;
      });
    return this.#capturing;
  }
}
