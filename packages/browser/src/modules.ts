import {
  FrameError,
  type Awaitable,
  type Cleanup,
  type Lifetime,
  type Release,
  type Unit,
} from "@frame/sdk";
import type { LocalPlan, PlannedUnit } from "@frame/sdk/protocol";
import { sameAuthority } from "./control.js";

export interface ModuleLease {
  readonly url: string;
  readonly digest: string;
  readonly closed?: Promise<void>;
  close(released: Cleanup): Awaitable<void | Release>;
}

interface Evaluation {
  readonly release: string;
  readonly authority: LocalPlan["authority"];
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
    const installation = unit.instance.source.installationId;
    const assertEvaluationDomain = () => {
      if (
        (this.#authority && !sameAuthority(this.#authority, plan.authority)) ||
        (this.#releases.has(installation) &&
          this.#releases.get(installation) !== unit.instance.source.release)
      )
        throw new FrameError(
          "reload_required",
          "evaluated installation requires a new realm after release or authority replacement",
        );
    };
    assertEvaluationDomain();
    const key = `${installation}/${unit.module.path}`;
    const previous = this.#evaluated.get(key);
    if (
      previous &&
      (previous.retired ||
        previous.release !== unit.instance.source.release ||
        !sameAuthority(previous.authority, plan.authority))
    ) {
      throw new FrameError(
        "reload_required",
        "evaluated module requires a new realm before replacement or reuse",
      );
    }
    let lease: ModuleLease | undefined;
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
    }, `module:${unit.module.path}`);
    await owner.run(async (signal) => {
      lease = await this.#open(unit, signal);
      const captured = lease;
      if (captured.closed) {
        const ended = () => {
          if (lease === captured) owner.beginClose();
        };
        void captured.closed.then(ended, ended);
      }
    });
    owner.assertOpen();
    assertEvaluationDomain();
    if (lease!.digest !== unit.module.digest)
      throw new FrameError(
        "contract_mismatch",
        "leased module does not match the approved artifact digest",
      );
    const url = new URL(lease!.url);
    if (!["https:", "http:", "file:", "data:"].includes(url.protocol))
      throw new FrameError(
        "permission_denied",
        "module lease uses an unsupported URL scheme",
      );
    evaluation = this.#evaluated.get(key);
    if (
      evaluation &&
      (evaluation.retired ||
        evaluation.release !== unit.instance.source.release ||
        !sameAuthority(evaluation.authority, plan.authority))
    )
      throw new FrameError(
        "reload_required",
        "module acquisition completed after its evaluation domain retired",
      );
    if (!evaluation) {
      // The browser owns ESM evaluation. An attempted evaluation is never made
      // retryable by deleting a Promise or appending a cache-busting query.
      const promise = import(/* @vite-ignore */ url.href) as Promise<
        Record<string, unknown>
      >;
      this.#authority = plan.authority;
      this.#releases.set(installation, unit.instance.source.release);
      evaluation = {
        release: unit.instance.source.release,
        authority: plan.authority,
        promise,
        leases: 0,
        retired: false,
      };
      this.#evaluated.set(key, evaluation);
    }
    evaluation.leases++;
    let exports: Record<string, unknown>;
    try {
      exports = await observeEvaluation(evaluation.promise, owner.signal);
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

// ESM evaluation belongs to the Realm and cannot be cancelled or unloaded. A
// retiring execution stops observing it, releases its lease, and refuses late
// exports. The evaluation remains in the Realm cache until a controlled reload.
function observeEvaluation<T>(
  evaluation: Promise<T>,
  signal: AbortSignal,
): Promise<T> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(signal.reason);
      return;
    }
    const abort = () => {
      signal.removeEventListener("abort", abort);
      reject(signal.reason);
    };
    signal.addEventListener("abort", abort, { once: true });
    evaluation.then(
      (value) => {
        signal.removeEventListener("abort", abort);
        resolve(value);
      },
      (error: unknown) => {
        signal.removeEventListener("abort", abort);
        reject(error);
      },
    );
  });
}
