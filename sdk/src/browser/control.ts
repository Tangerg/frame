import type {
  AcquireRequest,
  Authority,
  CloseProof,
  LocalPlan,
  ProofAcknowledgement,
  PublicationRequest,
  PublicationResult,
  ReadyReport,
  ReconnectRequest,
  ReconnectResult,
  Poll,
  RouteGrant,
  Snapshot,
  UseGrant,
  Withdrawal,
} from "../protocol.generated.js";

/** The application supplies an authenticated transport to its single Go Host. */
export interface HostControl {
  validatePlan(plan: LocalPlan): Promise<void>;
  ready(report: ReadyReport): Promise<PublicationResult>;
  acquire(request: AcquireRequest): Promise<UseGrant>;
  validateUse(grant: UseGrant): Promise<void>;
  validateRoute(grant: RouteGrant): Promise<void>;
  snapshot(points: readonly string[]): Promise<Snapshot>;
  closeSnapshot(snapshotId: string): Promise<void>;
  publish(request: PublicationRequest): Promise<PublicationResult>;
  publication(operationId: string): Promise<PublicationResult>;
  withdraw(request: Withdrawal): Promise<PublicationResult>;
  prove(proof: CloseProof): Promise<ProofAcknowledgement>;
  reconnect(request: ReconnectRequest): Promise<ReconnectResult>;
  poll(): Promise<Poll>;
}

export function sameAuthority(left: Authority, right: Authority): boolean {
  return left.id === right.id && left.epoch === right.epoch;
}
