import { expect, it, vi } from "vitest";
import { contribution, defineUnit } from "../index.js";
import type { PublicationResult } from "../protocol.generated.js";
import { additions, fixture, plan, unit } from "./runtime.test.fixture.js";

function publicationFixture() {
  const setup = vi.fn(() => ({
    additions: contribution({ title: "base" }, { value: "base" }),
  }));
  const contributor = defineUnit({
    id: "contributor",
    imports: {},
    exports: { additions },
    setup,
  });
  const approved = plan("decisions", [
    unit("contributor", "contributor", [
      { name: "additions", contract: additions.ref },
    ]),
  ]);
  return { ...fixture({ contributor }), approved, setup };
}

it.each([
  { name: "rejected", decision: "rejected" },
  { name: "unknown", decision: "unknown" },
  { name: "accepted with refusal", decision: "accepted", problem: "refused" },
] as const)(
  "retains a publication when a $name acknowledgement also claims acceptance",
  async (scenario) => {
    const f = publicationFixture();
    await f.runtime.prepare(f.approved);
    let accepted: PublicationResult | undefined;
    vi.mocked(f.control.publish).mockImplementation(async (request) => {
      accepted = {
        decision: "accepted",
        publication: {
          id: "publication",
          operationId: request.operationId,
          revision: "1",
          object: request.object,
        },
      };
      return {
        ...accepted,
        decision: scenario.decision,
        ...("problem" in scenario ? { problem: scenario.problem } : {}),
      };
    });
    const publication = await f.runtime.publish(
      f.approved.execution,
      "contributor",
      additions,
      "dynamic",
      { title: "dynamic" },
      { value: "retained" },
    );
    try {
      expect(publication.outcome.decision).toBe("unknown");
      expect(publication.outcome.publication).toBeUndefined();
      vi.mocked(f.control.publication).mockResolvedValue(accepted!);
      expect((await publication.observe()).decision).toBe("accepted");
      expect((await publication.withdraw()).decision).toBe("accepted");
    } finally {
      await f.runtime.close();
    }
  },
);

it("does not accept a publication identity as a Ready acknowledgement", async () => {
  const f = publicationFixture();
  vi.mocked(f.control.ready).mockImplementation(async (report) => {
    const result: PublicationResult = {
      decision: "accepted",
      publication: {
        id: "foreign",
        operationId: report.operationId,
        revision: "1",
        object: report.objects[0]!,
      },
    };
    return result;
  });
  try {
    await expect(f.runtime.prepare(f.approved)).rejects.toMatchObject({
      code: "outcome_unknown",
    });
    vi.mocked(f.control.ready).mockResolvedValue({ decision: "accepted" });
    await f.runtime.prepare(f.approved);
    expect(f.setup).toHaveBeenCalledOnce();
  } finally {
    await f.runtime.close();
  }
});

it("does not release a publication from another publication's withdrawal acknowledgement", async () => {
  const f = publicationFixture();
  await f.runtime.prepare(f.approved);
  const publication = await f.runtime.publish(
    f.approved.execution,
    "contributor",
    additions,
    "dynamic",
    { title: "dynamic" },
    { value: "retained" },
  );
  const result: PublicationResult = {
    decision: "accepted",
    publication: { ...publication.outcome.publication!, id: "foreign" },
  };
  vi.mocked(f.control.withdraw).mockResolvedValue(result);
  try {
    await expect(publication.withdraw()).rejects.toMatchObject({
      code: "outcome_unknown",
    });
    expect(f.runtime.pendingProofs).toHaveLength(0);
    vi.mocked(f.control.withdraw).mockResolvedValue({ decision: "accepted" });
    await publication.withdraw();
    expect(f.runtime.pendingProofs).toHaveLength(1);
  } finally {
    await f.runtime.close();
  }
});
