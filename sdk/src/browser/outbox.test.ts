import { describe, expect, it, vi } from "vitest";
import type {
  CloseProof,
  ProofAcknowledgement,
} from "../protocol.generated.js";
import { ControlChannel, type HostControl } from "./control.js";
import { ProofOutbox } from "./outbox.js";

const authority = { id: "host", epoch: "original" };

function proof(proofId: string, closeId = "original-close"): CloseProof {
  return {
    closeId,
    proofId,
    authority,
    realm: { id: "window", epoch: "original" },
    execution: { id: "execution", generation: "1" },
    scopeId: "scope",
    scopeGeneration: "1",
    scopeKind: "execution",
    instances: ["instance"],
    status: "released",
  };
}

function acknowledge(value: CloseProof): ProofAcknowledgement {
  return {
    authority,
    closeId: value.closeId,
    proofId: value.proofId,
    recorded: true,
  };
}

function channel(
  prove: HostControl["prove"] = async (value) => acknowledge(value),
): ControlChannel {
  return new ControlChannel(authority, { prove } as HostControl);
}

describe("proof reservation ownership", () => {
  it.each([
    ["another proof identity", proof("replacement")],
    ["changed original evidence", proof("original", "changed-close")],
  ] as const)(
    "rejects %s for a scope that already owns evidence",
    (_label, replacement) => {
      const outbox = new ProofOutbox(1);
      const original = proof("original");
      outbox.reserve("scope");
      outbox.record("scope", original);
      expect(() => outbox.record("scope", replacement)).toThrow(
        expect.objectContaining({ code: "invalid_plan" }),
      );
      expect(outbox.pending).toEqual([original]);
      expect(outbox.reserved("scope")).toBe(true);
      expect(outbox.empty).toBe(false);
    },
  );

  it("cannot report empty while a competing proof for the same scope remains pending", async () => {
    const outbox = new ProofOutbox(1);
    const original = proof("original"),
      competing = proof("competing");
    outbox.reserve("scope");
    outbox.record("scope", original);
    let refused: unknown;
    try {
      outbox.record("scope", competing);
    } catch (error) {
      refused = error;
    }
    const pending = Promise.withResolvers<ProofAcknowledgement>();
    const observed = Promise.withResolvers<{
      empty: boolean;
      pending: readonly CloseProof[];
    }>();
    const sending = outbox.flush(
      channel(async (value) =>
        value.proofId === original.proofId
          ? acknowledge(value)
          : pending.promise,
      ),
      () => {
        observed.resolve({ empty: outbox.empty, pending: outbox.pending });
      },
    );
    try {
      expect(await observed.promise).toEqual({ empty: true, pending: [] });
      expect(refused).toMatchObject({ code: "invalid_plan" });
    } finally {
      pending.resolve(acknowledge(competing));
      await sending;
    }
  });

  it("allows the same immutable proof to replay and rejects its identity in another reserved scope", async () => {
    const outbox = new ProofOutbox(2);
    const original = proof("original");
    outbox.reserve("original-scope");
    outbox.reserve("other-scope");
    outbox.record("original-scope", original);
    outbox.record("original-scope", { ...original });
    expect(() => outbox.record("other-scope", { ...original })).toThrow(
      expect.objectContaining({ code: "invalid_plan" }),
    );
    expect(outbox.pending).toEqual([original]);
    const accepted = vi.fn();
    await outbox.flush(channel(), accepted);
    expect(accepted).toHaveBeenCalledExactlyOnceWith(original);
    expect(outbox.reserved("original-scope")).toBe(false);
    expect(outbox.reserved("other-scope")).toBe(true);
    expect(outbox.empty).toBe(false);
    outbox.cancelReservation("other-scope");
    expect(outbox.empty).toBe(true);
  });

  it("retains capacity for both uncreated and recorded scopes until each is settled", async () => {
    const outbox = new ProofOutbox(2);
    outbox.reserve("uncreated");
    outbox.reserve("recorded");
    outbox.record("recorded", proof("original"));
    expect(() => outbox.reserve("extra")).toThrow(
      expect.objectContaining({ code: "capacity" }),
    );
    expect(() => outbox.cancelReservation("recorded")).toThrow(
      expect.objectContaining({ code: "invalid_plan" }),
    );
    outbox.cancelReservation("uncreated");
    outbox.reserve("extra");
    await outbox.flush(channel(), () => {});
    expect(outbox.pending).toEqual([]);
    expect(outbox.empty).toBe(false);
    outbox.cancelReservation("extra");
    expect(outbox.empty).toBe(true);
  });

  it("delivers an earlier reservation that becomes ready while another proof is awaiting acknowledgement", async () => {
    const outbox = new ProofOutbox(2);
    const first = proof("first"),
      earlier = proof("earlier");
    outbox.reserve("earlier-scope");
    outbox.reserve("first-scope");
    outbox.record("first-scope", first);
    const acknowledgement = Promise.withResolvers<ProofAcknowledgement>();
    const prove = vi.fn<HostControl["prove"]>(async (value) =>
      value.proofId === first.proofId
        ? acknowledgement.promise
        : acknowledge(value),
    );
    const sending = outbox.flush(channel(prove), () => {});
    expect(prove).toHaveBeenCalledExactlyOnceWith(
      first,
      expect.any(AbortSignal),
    );
    outbox.record("earlier-scope", earlier);
    acknowledgement.resolve(acknowledge(first));
    await sending;
    expect(prove.mock.calls.map(([value]) => value.proofId)).toEqual([
      first.proofId,
      earlier.proofId,
    ]);
    expect(outbox.pending).toEqual([]);
    expect(outbox.empty).toBe(true);
  });

  it.each(["reserved", "recorded"] as const)(
    "ignores a retired acknowledgement after the same scope becomes %s again",
    async (state) => {
      const outbox = new ProofOutbox(1);
      const original = proof("original"),
        next = proof("next", "next-close");
      outbox.reserve("scope");
      outbox.record("scope", original);
      const late = Promise.withResolvers<ProofAcknowledgement>();
      const accepted = vi.fn();
      const retired = outbox.flush(
        channel(() => late.promise),
        accepted,
      );
      const current = channel();
      await outbox.flush(current, accepted);
      await retired;
      outbox.reserve("scope");
      if (state === "recorded") outbox.record("scope", next);
      late.resolve(acknowledge(original));
      await late.promise;
      expect(outbox.reserved("scope")).toBe(true);
      expect(outbox.empty).toBe(false);
      expect(outbox.pending).toEqual(state === "recorded" ? [next] : []);
      expect(accepted).toHaveBeenCalledExactlyOnceWith(original);
      if (state === "reserved") outbox.record("scope", next);
      await outbox.flush(current, accepted);
      expect(accepted).toHaveBeenCalledTimes(2);
      expect(accepted).toHaveBeenLastCalledWith(next);
      expect(outbox.empty).toBe(true);
    },
  );
});

it("does not join an empty flush after the first proof becomes ready in the same turn", async () => {
  const outbox = new ProofOutbox(1);
  const original = proof("original");
  outbox.reserve("scope");
  const acknowledgement = Promise.withResolvers<ProofAcknowledgement>();
  const prove = vi.fn<HostControl["prove"]>(() => acknowledgement.promise);
  const current = channel(prove);
  const first = outbox.flush(current, () => {});
  outbox.record("scope", original);
  let completed = false;
  const second = outbox
    .flush(current, () => {})
    .then(() => {
      completed = true;
    });
  try {
    await first;
    expect(prove).toHaveBeenCalledExactlyOnceWith(
      original,
      expect.any(AbortSignal),
    );
    expect(completed).toBe(false);
  } finally {
    acknowledgement.resolve(acknowledge(original));
    await second;
  }
  expect(outbox.empty).toBe(true);
});

it("delivers proofs in recording order even when reservations were made in reverse order", async () => {
  const outbox = new ProofOutbox(2);
  const execution = proof("execution");
  const use: CloseProof = { ...proof("use"), scopeKind: "use", useId: "use" };
  outbox.reserve("execution-scope");
  outbox.reserve("use-scope");
  outbox.record("use-scope", use);
  outbox.record("execution-scope", execution);
  outbox.record("use-scope", { ...use });
  let loseAcknowledgement = true;
  const prove = vi.fn<HostControl["prove"]>(async (value) => {
    if (value.proofId === execution.proofId && loseAcknowledgement) {
      loseAcknowledgement = false;
      throw new Error("execution acknowledgement lost");
    }
    return acknowledge(value);
  });
  const current = channel(prove);
  await expect(outbox.flush(current, () => {})).rejects.toThrow(
    "execution acknowledgement lost",
  );
  expect(prove.mock.calls.map(([value]) => value.proofId)).toEqual([
    use.proofId,
    execution.proofId,
  ]);
  expect(outbox.pending).toEqual([execution]);
  await outbox.flush(current, () => {});
  expect(prove.mock.calls.map(([value]) => value.proofId)).toEqual([
    use.proofId,
    execution.proofId,
    execution.proofId,
  ]);
  expect(outbox.empty).toBe(true);
});
