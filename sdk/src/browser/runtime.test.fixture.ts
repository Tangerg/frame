import { vi } from "vitest";
import { FrameError, Point, Service, type Unit } from "../index.js";
import {
  PROTOCOL,
  type CloseProof,
  type ContractRef,
  type LocalPlan,
  type PlannedUnit,
  type PublicationResult,
  type ReadyReport,
  type UseGrant,
} from "../protocol.generated.js";
import { Runtime } from "./runtime.js";
import type { HostControl } from "./control.js";

export const digest = "a".repeat(64);
export const authority = { id: "host", epoch: "host-1" };
export const realm = { id: "window", epoch: "window-1" };
export const ref = (
  name: string,
  kind: ContractRef["kind"] = "service",
): ContractRef => ({ name, version: "1", digest, kind, mode: "native" });
export const files = new Service<{ read(): string }>(ref("documents.files"));
export const preview = new Service<{ text(): string }>(
  ref("documents.preview"),
);
export const additions = new Point<{ title: string }, { value: string }>(
  ref("documents.additions", "point"),
);

export function unit(
  id: string,
  name: string,
  exports: PlannedUnit["exports"],
  imports: PlannedUnit["imports"] = [],
  value = id,
): PlannedUnit {
  return {
    instance: {
      id,
      generation: "1",
      source: { installationId: id, release: digest, unitId: name },
      configuration: digest,
    },
    configurationJson: JSON.stringify(value),
    profile: "browser-js",
    module: { path: name, digest },
    imports,
    exports,
  };
}

export function plan(id: string, units: readonly PlannedUnit[]): LocalPlan {
  return {
    protocol: PROTOCOL,
    approvalToken: `plan:${id}`,
    authority,
    realm,
    execution: { id, generation: "1" },
    scopeId: id,
    scopeGeneration: "1",
    units,
  };
}

export function fixture(
  definitions: Record<string, Unit>,
  proofCapacity = 128,
) {
  const ready = new Map<string, ReadyReport>();
  const grants = new Map<string, UseGrant>();
  const publications = new Map<string, PublicationResult>();
  const proofs = new Map<string, CloseProof>();
  let nextGrant = 0;
  const control: HostControl = {
    admitPlan: vi.fn<HostControl["admitPlan"]>(async (value) => {
      if (value.approvalToken !== `plan:${value.execution.id}`)
        throw new Error("invalid plan approval");
      return {
        authority,
        realm,
        execution: value.execution,
        decision: "admitted",
      };
    }),
    ready: vi.fn<HostControl["ready"]>(async (report) => {
      ready.set(report.execution.id, report);
      return { decision: "accepted" };
    }),
    acquire: vi.fn<HostControl["acquire"]>(async () => {
      throw new Error("fixture requires an explicit grant");
    }),
    validateUse: vi.fn<HostControl["validateUse"]>(async (grant) => {
      if (JSON.stringify(grants.get(grant.id)) !== JSON.stringify(grant))
        throw new FrameError("stale_binding", "invalid use approval");
    }),
    validateRoute: vi.fn<HostControl["validateRoute"]>(async () => {}),
    snapshot: vi.fn<HostControl["snapshot"]>(async () => ({
      id: "snapshot",
      authority,
      points: [],
    })),
    closeSnapshot: vi.fn<HostControl["closeSnapshot"]>(async () => {}),
    publish: vi.fn<HostControl["publish"]>(async (request) => {
      const result: PublicationResult = {
        decision: "accepted",
        publication: {
          id: `publication:${request.operationId}`,
          operationId: request.operationId,
          revision: "1",
          object: request.object,
        },
      };
      publications.set(request.operationId, result);
      return result;
    }),
    publication: vi.fn<HostControl["publication"]>(
      async (id) => publications.get(id) ?? { decision: "unknown" },
    ),
    withdraw: vi.fn<HostControl["withdraw"]>(async () => ({
      decision: "accepted",
    })),
    prove: vi.fn<HostControl["prove"]>(async (proof) => {
      const previous = proofs.get(proof.proofId);
      if (previous && JSON.stringify(previous) !== JSON.stringify(proof))
        throw new Error("proof changed");
      proofs.set(proof.proofId, proof);
      if (proof.useId) grants.delete(proof.useId);
      return {
        closeId: proof.closeId,
        proofId: proof.proofId,
        recorded: true,
        authority,
      };
    }),
    poll: vi.fn<HostControl["poll"]>(async () => ({
      authority,
      realm,
      plans: [],
      fences: [],
    })),
    reconnect: vi.fn<HostControl["reconnect"]>(async (request) => ({
      authority,
      realm,
      mode: "revalidated",
      executions: request.executions.map((execution) => ({
        execution,
        scopeId: execution.id,
        scopeGeneration: "1",
      })),
    })),
  };
  const runtime = new Runtime({
    authority,
    realm,
    control,
    proofCapacity,
    load: (entry) => definitions[entry.module.path]!,
    remote: () => {
      throw new Error("unexpected remote import");
    },
  });
  function grant(
    id: string,
    objects = ready.get(id)!.objects,
    operationId = `operation:${nextGrant + 1}`,
    parentUseId?: string,
  ): UseGrant {
    const value: UseGrant = {
      id: `use:${++nextGrant}`,
      operationId,
      ...(parentUseId ? { parentUseId } : {}),
      approvalToken: `grant:${nextGrant}`,
      authority,
      realm,
      execution: { id, generation: "1" },
      scopeId: id,
      scopeGeneration: "1",
      objects,
    };
    grants.set(value.id, value);
    return value;
  }
  return { runtime, control, ready, grants, grant, publications, proofs };
}
