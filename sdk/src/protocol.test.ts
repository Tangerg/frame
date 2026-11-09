import { describe, expect, expectTypeOf, it } from "vitest";
import {
  decodeControlOutput,
  decodeProtocolValue,
  type ProtocolValues,
  type ReadyReport,
} from "./protocol.generated.js";

const authority = { id: "host", epoch: "1" };
const realm = { id: "realm", epoch: "1" };
const execution = { id: "execution", generation: "1" };

describe("generated protocol admission", () => {
  it("admits captured input values through the Go-owned wire model", () => {
    const value: unknown = {
      operationId: "ready",
      authority,
      realm,
      execution,
      objects: [],
      contributions: [],
    };
    const report = decodeProtocolValue("ReadyReport", value);
    expectTypeOf(report).toEqualTypeOf<ReadyReport>();
    expect(report).toBe(value);
    expect(() =>
      decodeProtocolValue("ReadyReport", { ...report, objects: null }),
    ).toThrow(TypeError);
    expect(() =>
      decodeProtocolValue("ReadyReport", { ...report, extra: true }),
    ).toThrow(TypeError);
  });

  it.each(["plans", "fences"] as const)(
    "rejects an omitted element of a direct %s response",
    (field) => {
      const value = {
        authority,
        realm,
        plans: [],
        fences: [],
        [field]: Array(1),
      };
      expect(() => decodeControlOutput("poll", value)).toThrow(TypeError);
    },
  );

  it("does not accept inherited array elements as wire data", () => {
    const fences = Array(1);
    Object.setPrototypeOf(fences, {
      0: { scopeId: "scope", scopeGeneration: "1" },
    });
    expect(() =>
      decodeControlOutput("poll", { authority, realm, plans: [], fences }),
    ).toThrow(TypeError);
  });

  it("rejects sparse request arrays before a typed capture is published", () => {
    expect(() =>
      decodeProtocolValue("ReadyReport", {
        operationId: "ready",
        authority,
        realm,
        execution,
        objects: Array(1),
        contributions: [],
      }),
    ).toThrow(TypeError);
  });

  it("does not expose prototype properties as protocol models", () => {
    expect(() =>
      decodeProtocolValue("toString" as keyof ProtocolValues, {}),
    ).toThrow(TypeError);
  });
});
