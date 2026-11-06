import { expect, it } from "vitest";
import type { ObjectRef, PublicationResult } from "../protocol.generated.js";
import { PublicationReceipt } from "./publication.js";

it("validates and retains one captured publication acknowledgement", () => {
  const object: ObjectRef = {
    objectId: "object",
    export: {
      instanceId: "instance",
      name: "entry",
      contract: {
        name: "example.point",
        version: "1",
        digest: "a".repeat(64),
        kind: "point",
        mode: "native",
      },
    },
  };
  const receipt = new PublicationReceipt("operation", object);
  let reads = 0;
  const response: PublicationResult = {
    get decision() {
      return ++reads === 1 ? "accepted" : "rejected";
    },
    publication: {
      id: "publication",
      operationId: "operation",
      revision: "1",
      object,
    },
  };
  expect(receipt.receive(response).decision).toBe("accepted");
  expect(reads).toBe(1);
  expect(Object.isFrozen(receipt.outcome)).toBe(true);
});
