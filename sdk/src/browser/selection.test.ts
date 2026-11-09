import { expect, it } from "vitest";
import { Point } from "../contracts.js";
import type { Snapshot } from "../protocol.generated.js";
import { select } from "./selection.js";

interface Priority {
  readonly priority: number;
}

function fixture() {
  const point = new Point<Priority, string>({
    name: "priority",
    version: "1",
    digest: "a".repeat(64),
    kind: "point",
    mode: "native",
  });
  const candidate = {
    publicationId: "publication",
    source: {
      installationId: "installed",
      release: "b".repeat(64),
      unitId: "provider",
      export: "entry",
      contract: point.ref,
    },
    token: "original-token",
    metadataJson: '{"priority":1}',
  };
  const snapshot: Snapshot = {
    id: "snapshot",
    authority: { id: "host", epoch: "1" },
    points: [{ point: point.ref, revision: "1", candidates: [candidate] }],
  };
  return { point, candidate, snapshot };
}

function decodePriority(value: unknown): Priority {
  if (
    value === null ||
    typeof value !== "object" ||
    !("priority" in value) ||
    typeof value.priority !== "number"
  )
    throw new TypeError("priority metadata is required");
  return { priority: value.priority };
}

it("uses the same captured metadata for a candidate and its selection policy", () => {
  const { point, candidate, snapshot } = fixture();
  let reads = 0;
  Object.defineProperty(candidate, "metadataJson", {
    get: () => JSON.stringify({ priority: ++reads === 1 ? 1 : 999 }),
  });

  const selection = select(
    snapshot,
    point,
    "minimum-priority",
    (choices) => {
      expect(choices[0]!.metadata).toEqual({ priority: 1 });
      expect(choices[0]!.candidate.metadataJson).toBe('{"priority":1}');
      return choices.filter(({ metadata }) => metadata.priority >= 100);
    },
    decodePriority,
  );

  expect(reads).toBe(1);
  expect(selection.candidates).toEqual([]);
});

it("captures every candidate before metadata decoding can change the input", () => {
  const { point, candidate, snapshot } = fixture();
  const next = {
    ...candidate,
    token: "second-token",
    metadataJson: '{"priority":2}',
  };
  const input: Snapshot = {
    ...snapshot,
    points: [{ ...snapshot.points[0]!, candidates: [candidate, next] }],
  };
  const selection = select(
    input,
    point,
    "all-priorities",
    (choices) => {
      expect(choices.map(({ metadata }) => metadata.priority)).toEqual([1, 2]);
      expect(Object.isFrozen(choices[1]!.candidate)).toBe(true);
      return choices;
    },
    (value) => {
      next.token = "replacement-token";
      next.metadataJson = '{"priority":999}';
      return decodePriority(value);
    },
  );

  expect(selection.candidates).toEqual(["original-token", "second-token"]);
  expect(next.token).toBe("replacement-token");
});
