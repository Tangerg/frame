import { captureProtocol } from "../protocol-capture.js";
import { Point, FrameError, sameContract } from "../index.js";
import { parseJSON } from "../json.js";
import type { Candidate, Selection, Snapshot } from "../protocol.generated.js";

export interface Choice<M> {
  readonly candidate: Candidate;
  readonly metadata: M;
}

function frozen<T>(value: T): T {
  if (value && typeof value === "object" && !Object.isFrozen(value)) {
    Object.freeze(value);
    for (const child of Object.values(value)) frozen(child);
  }
  return value;
}

/** Policy runs in the consumer; only Host admission can turn this data into Use. */
export function select<M, V>(
  snapshot: Snapshot,
  point: Point<M, V>,
  rule: string,
  choose: (choices: readonly Choice<M>[]) => readonly Choice<M>[],
  decode: (value: unknown) => M,
): Selection {
  const captured = captureProtocol("Snapshot", snapshot).points.find((entry) =>
    sameContract(entry.point, point.ref),
  );
  if (!captured)
    throw new FrameError(
      "contract_mismatch",
      "point was not captured in this host snapshot",
    );
  if (!rule)
    throw new TypeError("selection rule must have an explicit identity");
  const choices = Object.freeze(
    captured.candidates.map((candidate) =>
      Object.freeze({
        candidate,
        metadata: frozen(decode(parseJSON(candidate.metadataJson))),
      }),
    ),
  );
  const candidates: string[] = [];
  const seen = new Set<Choice<M>>();
  for (const entry of choose(choices)) {
    if (!choices.includes(entry) || seen.has(entry))
      throw new FrameError(
        "stale_binding",
        "selection contains a foreign or repeated candidate",
      );
    seen.add(entry);
    candidates.push(entry.candidate.token);
  }
  return Object.freeze({
    point: point.ref,
    candidates: Object.freeze(candidates),
    rule,
  });
}
