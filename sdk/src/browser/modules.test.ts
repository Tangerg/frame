import { describe, expect, it, vi } from "vitest";
import { Lifetime, type Cleanup } from "../index.js";
import {
  PROTOCOL,
  type LocalPlan,
  type PlannedUnit,
} from "../protocol.generated.js";
import { RealmModules } from "./modules";

const digest = "a".repeat(64);
function unit(path = "module.js", release = digest): PlannedUnit {
  return {
    instance: {
      id: crypto.randomUUID(),
      generation: "1",
      source: { installationId: "documents", release, unitId: "preview" },
      configuration: digest,
    },
    configurationJson: "null",
    profile: "browser-js",
    module: { path, digest },
    imports: [],
    exports: [],
  };
}
function plan(value: PlannedUnit, epoch = "1"): LocalPlan {
  return {
    protocol: PROTOCOL,
    approvalToken: "approved",
    authority: { id: "host", epoch },
    realm: { id: "window", epoch: "1" },
    execution: { id: "execution", generation: "1" },
    scopeId: "scope",
    scopeGeneration: "1",
    units: [value],
  };
}
const source =
  "data:text/javascript," +
  encodeURIComponent(
    "export default {id:'preview',imports:{},exports:{},setup(){return {}}}",
  );

describe("Realm module ownership", () => {
  it("evaluates once and retires only after every actual lease releases", async () => {
    const close = vi.fn();
    const modules = new RealmModules(async () => ({
      url: source,
      digest,
      close,
    }));
    const first = new Lifetime("first");
    const second = new Lifetime("second");
    const entry = unit();
    expect(await modules.load(entry, first, plan(entry))).toBe(
      await modules.load(entry, second, plan(entry)),
    );
    await first.close();
    expect(close).toHaveBeenCalledOnce();
    await second.close();
    const next = new Lifetime("replacement");
    await expect(modules.load(entry, next, plan(entry))).rejects.toMatchObject({
      code: "reload_required",
    });
    await next.close();
  });

  it("requires a new Realm for a release replacement even when the entry path changed", async () => {
    const open = vi.fn(async () => ({ url: source, digest, close() {} }));
    const modules = new RealmModules(open);
    const first = new Lifetime("first");
    const entry = unit();
    await modules.load(entry, first, plan(entry));
    const replacement = unit("other.js", "b".repeat(64));
    const next = new Lifetime("next");
    await expect(
      modules.load(replacement, next, plan(replacement)),
    ).rejects.toMatchObject({ code: "reload_required" });
    await expect(
      modules.load(entry, next, plan(entry, "2")),
    ).rejects.toMatchObject({ code: "reload_required" });
    expect(open).toHaveBeenCalledOnce();
    await first.close();
    await next.close();
  });

  it("accepts late lease evidence without replaying failed cleanup", async () => {
    let confirmation!: Cleanup;
    const close = vi.fn((released: Cleanup) => {
      confirmation = released;
      return { status: "unconfirmed" as const };
    });
    const modules = new RealmModules(async () => ({
      url: source,
      digest,
      close,
    }));
    const owner = new Lifetime("owner");
    const entry = unit();
    await modules.load(entry, owner, plan(entry));
    expect((await owner.close()).status).toBe("unconfirmed");
    confirmation.confirmReleased(
      "lease endpoint confirmed the original closure",
    );
    expect((await owner.wait()).status).toBe("released");
    expect(close).toHaveBeenCalledOnce();
    const next = new Lifetime("next");
    await expect(modules.load(entry, next, plan(entry))).rejects.toMatchObject({
      code: "reload_required",
    });
    await next.close();
  });

  it("releases execution leases while Realm-owned ESM is pending and refuses late exports", async () => {
    const started = Promise.withResolvers<void>();
    const resume = Promise.withResolvers<void>();
    const key = `frameEvaluation${crypto.randomUUID().replaceAll("-", "")}`;
    vi.stubGlobal(key, { start: started.resolve, resume: resume.promise });
    const url =
      "data:text/javascript," +
      encodeURIComponent(
        `globalThis["${key}"].start(); await globalThis["${key}"].resume; export default {id:'preview',imports:{},exports:{},setup(){return {}}}`,
      );
    const close = vi.fn();
    const modules = new RealmModules(async () => ({ url, digest, close }));
    const owner = new Lifetime("evaluating");
    const entry = unit();
    const loading = owner
      .run(() => modules.load(entry, owner, plan(entry)))
      .catch((error: unknown) => error);
    try {
      await started.promise;
      expect((await owner.close()).status).toBe("released");
      expect(close).toHaveBeenCalledOnce();
      expect(await loading).toBe(owner.signal.reason);
    } finally {
      resume.resolve();
      await import(/* @vite-ignore */ url);
      vi.unstubAllGlobals();
    }
  });

  it("joins an in-flight acquisition and releases a lease that arrives after cancellation", async () => {
    const pending = Promise.withResolvers<{
      url: string;
      digest: string;
      close(): void;
    }>();
    const close = vi.fn();
    const modules = new RealmModules(() => pending.promise);
    const owner = new Lifetime("acquiring");
    const entry = unit();
    const loading = owner
      .run(() => modules.load(entry, owner, plan(entry)))
      .catch((error: unknown) => error);
    let closed = false;
    const retiring = owner.close().then((outcome) => {
      closed = true;
      return outcome;
    });
    await Promise.resolve();
    expect(closed).toBe(false);
    pending.resolve({ url: source, digest, close });
    expect((await retiring).status).toBe("released");
    expect(close).toHaveBeenCalledOnce();
    expect(await loading).toMatchObject({ code: "closed" });
  });
});
