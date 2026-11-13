import {
  FrameError,
  type Awaitable,
  type Cleanup,
  type Lifetime,
  type Release,
  type Unit,
} from "../index.js";
import type { LocalPlan, PlannedUnit } from "../protocol.generated.js";
import { observePromise } from "../promise-observation.js";
import { sameAuthority } from "./identity.js";

export interface ModuleLease {
  readonly bytes: Uint8Array<ArrayBuffer>;
  readonly closed?: Promise<void>;
  close(released: Cleanup): Awaitable<void | Release>;
}

interface Evaluation {
  readonly digest: string;
  readonly promise: Promise<Record<string, unknown>>;
  leases: number;
  retired: boolean;
}

/** One instance belongs to the actual Realm, independently of individual plans. */
export class RealmModules {
  readonly #open: (
    unit: PlannedUnit,
    signal: AbortSignal,
  ) => Promise<ModuleLease>;
  readonly #evaluated = new Map<string, Evaluation>();
  readonly #artifacts = new Map<string, Evaluation>();
  readonly #releases = new Map<string, string>();
  #authority?: LocalPlan["authority"];

  constructor(
    open: (unit: PlannedUnit, signal: AbortSignal) => Promise<ModuleLease>,
  ) {
    this.#open = open;
  }

  async evaluate(
    unit: PlannedUnit,
    owner: Lifetime,
    plan: LocalPlan,
  ): Promise<Readonly<Record<string, unknown>>> {
    owner.assertOpen();
    const { installationId: installation, release } = unit.instance.source;
    const { path, digest } = unit.module;
    const authority = Object.freeze({ ...plan.authority });
    const assertEvaluationDomain = () => {
      if (
        (this.#authority && !sameAuthority(this.#authority, authority)) ||
        (this.#releases.has(installation) &&
          this.#releases.get(installation) !== release)
      )
        throw new FrameError(
          "reload_required",
          "evaluated installation requires a new realm after release or authority replacement",
        );
    };
    const key = JSON.stringify([installation, path]);
    const currentEvaluation = () => {
      assertEvaluationDomain();
      const current = this.#artifacts.get(key);
      if (current && (current.retired || current.digest !== digest))
        throw new FrameError(
          "reload_required",
          "evaluated module cannot be replaced or revived in this realm",
        );
      return current;
    };
    currentEvaluation();
    let lease: Pick<ModuleLease, "close"> | undefined;
    let evaluation: Evaluation | undefined;
    const retire = () => {
      if (!lease) return;
      lease = undefined;
      if (evaluation) {
        evaluation.leases--;
        if (!evaluation.leases) evaluation.retired = true;
      }
    };
    const cleanup = owner.cleanup(async () => {
      if (!lease) return;
      const result = await lease.close({
        confirmReleased(evidence) {
          cleanup.confirmReleased(evidence);
          retire();
        },
      });
      if (result === undefined || result.status === "released") {
        retire();
      }
      return result;
    }, `module:${path}`);
    const bytes = await owner.run(async (signal) => {
      const captured = await this.#open(unit, signal);
      lease = captured;
      const held = { close: captured.close.bind(captured) };
      lease = held;
      const bytes = new Uint8Array(captured.bytes);
      const closed = captured.closed;
      if (closed) {
        const ended = () => {
          if (lease === held) owner.beginClose();
        };
        void closed.then(ended, ended);
      }
      return bytes;
    });
    owner.assertOpen();
    const actualDigest = await owner.run(async () =>
      Array.from(
        new Uint8Array(await crypto.subtle.digest("SHA-256", bytes)),
        (value) => value.toString(16).padStart(2, "0"),
      ).join(""),
    );
    owner.assertOpen();
    if (actualDigest !== digest)
      throw new FrameError(
        "contract_mismatch",
        "module bytes do not match the approved artifact digest",
      );
    // Hash and evaluation consume the same detached bytes. Native import must
    // never fetch a mutable URL after a different response was verified.
    let binary = "";
    for (const byte of bytes) binary += String.fromCharCode(byte);
    const url = `data:text/javascript;base64,${btoa(binary)}`;
    currentEvaluation();
    evaluation = this.#evaluated.get(url);
    if (evaluation?.retired)
      throw new FrameError(
        "reload_required",
        "evaluated module cannot be revived through another artifact identity",
      );
    if (!evaluation) {
      // The browser owns ESM evaluation. An attempted evaluation is never made
      // retryable by deleting a Promise or appending a cache-busting query.
      const promise = import(/* @vite-ignore */ url) as Promise<
        Record<string, unknown>
      >;
      evaluation = {
        digest: digest,
        promise,
        leases: 0,
        retired: false,
      };
      this.#evaluated.set(url, evaluation);
    }
    this.#authority ??= authority;
    this.#releases.set(installation, release);
    this.#artifacts.set(key, evaluation);
    evaluation.leases++;
    let exports: Record<string, unknown>;
    try {
      // Native ESM cannot be cancelled or unloaded. Only this execution's
      // observation ends; the Realm retains its evaluation identity.
      exports = await observePromise(evaluation.promise, owner.signal);
    } catch (cause) {
      owner.signal.throwIfAborted();
      throw new FrameError(
        "reload_required",
        "module evaluation failed; the realm must be replaced",
        { cause },
      );
    }
    owner.assertOpen();
    return exports;
  }

  async load(
    unit: PlannedUnit,
    owner: Lifetime,
    plan: LocalPlan,
  ): Promise<Unit> {
    const exports = await this.evaluate(unit, owner, plan);
    const definition = exports.default;
    if (
      !definition ||
      typeof definition !== "object" ||
      !("setup" in definition) ||
      typeof definition.setup !== "function"
    )
      throw new FrameError(
        "contract_mismatch",
        "module default export must be a Frame unit declaration",
      );
    return definition as Unit;
  }
}
