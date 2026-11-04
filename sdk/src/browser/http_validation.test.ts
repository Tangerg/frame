import { describe, expect, it, vi } from "vitest";
import { Lifetime } from "../index.js";
import type { ReadyReport, UseGrant } from "../protocol.generated.js";
import { HTTPControl } from "./http.js";

const authority = { id: "host", epoch: "1" };
const realm = { id: "realm", epoch: "1" };
const execution = { id: "execution", generation: "1" };
const ready: ReadyReport = {
  operationId: "ready",
  authority,
  realm,
  execution,
  objects: [],
  contributions: [],
};
const grant: UseGrant = {
  operationId: "acquire",
  id: "use",
  approvalToken: "approval",
  authority,
  realm,
  execution,
  scopeId: "scope",
  scopeGeneration: "1",
  objects: [],
};

function transport(body: unknown) {
  const lifetime = new Lifetime("control response validation");
  const fetch = vi.fn<typeof globalThis.fetch>(async () => Response.json(body));
  const control = new HTTPControl({
    endpoint: "https://example.invalid/control",
    credential: "realm-secret",
    maximumMessageBytes: 4096,
    lifetime,
    fetch,
  });
  return { control, lifetime, fetch };
}

describe("control response admission", () => {
  it.each([{}, false, [], "accepted"])(
    "refuses a malformed validation acknowledgement %j",
    async (body) => {
      const f = transport(body);
      await expect(f.control.validateUse(grant)).rejects.toMatchObject({
        code: "unavailable",
      });
      await f.lifetime.close();
    },
  );

  it.each([
    null,
    {},
    { decision: "published" },
    { decision: "accepted", publication: null },
    { decision: "accepted", problem: 1 },
    { decision: "accepted", extra: true },
  ])("keeps a malformed ready result unknown: %j", async (body) => {
    const f = transport(body);
    await expect(f.control.ready(ready)).rejects.toMatchObject({
      code: "outcome_unknown",
    });
    expect(f.fetch).toHaveBeenCalledOnce();
    await f.lifetime.close();
  });

  it("does not confirm snapshot release from an arbitrary success body", async () => {
    const f = transport({});
    await expect(
      f.control.closeSnapshot("original-snapshot"),
    ).rejects.toMatchObject({
      code: "outcome_unknown",
    });
    await f.lifetime.close();
  });

  it.each([
    { authority, realm, plans: null, fences: [] },
    { authority, realm, plans: [], fences: [{ scopeId: "scope" }] },
    { authority: { ...authority, epoch: 1 }, realm, plans: [], fences: [] },
  ])("rejects malformed nested poll fields: %j", async (body) => {
    const f = transport(body);
    await expect(f.control.poll()).rejects.toMatchObject({
      code: "unavailable",
    });
    await f.lifetime.close();
  });

  it("accepts the declared replies without creating a second domain decision", async () => {
    for (const decision of ["accepted", "rejected", "unknown"] as const) {
      const f = transport({ decision });
      await expect(f.control.ready(ready)).resolves.toEqual({ decision });
      await f.lifetime.close();
    }
    const acknowledged = transport(null);
    await expect(
      acknowledged.control.validateUse(grant),
    ).resolves.toBeUndefined();
    await expect(
      acknowledged.control.closeSnapshot("snapshot"),
    ).resolves.toBeUndefined();
    await acknowledged.lifetime.close();
    const polled = transport({ authority, realm, plans: [], fences: [] });
    await expect(polled.control.poll()).resolves.toEqual({
      authority,
      realm,
      plans: [],
      fences: [],
    });
    await polled.lifetime.close();
  });
});
