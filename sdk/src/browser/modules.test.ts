import { describe, expect, it, vi } from "vitest";
import { createHash } from "node:crypto";
import { Lifetime, type Cleanup } from "../index.js";
import {
  PROTOCOL,
  type LocalPlan,
  type PlannedUnit,
} from "../protocol.generated.js";
import { RealmModules } from "./modules";

const source = new TextEncoder().encode(
  "export default {id:'preview',imports:{},exports:{},setup(){return {}}}",
);
const hash = (bytes: Uint8Array) =>
  createHash("sha256").update(bytes).digest("hex");
const digest = hash(source);
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

describe("Realm module ownership", () => {
  it("executes newly approved bytes at a reused artifact path", async () => {
    let bytes = new TextEncoder().encode("export default 'original bytes'");
    const modules = new RealmModules(async () => ({ bytes, close() {} }));
    const first = {
      ...unit("shared.js"),
      module: { path: "shared.js", digest: hash(bytes) },
    };
    const original = new Lifetime("original");
    expect((await modules.evaluate(first, original, plan(first))).default).toBe(
      "original bytes",
    );
    await original.close();

    bytes = new TextEncoder().encode("export default 'replacement bytes'");
    const next = {
      ...first,
      instance: {
        ...first.instance,
        source: { ...first.instance.source, installationId: "replacement" },
      },
      module: { path: "shared.js", digest: hash(bytes) },
    };
    const replacement = new Lifetime("replacement");
    expect(
      (await modules.evaluate(next, replacement, plan(next))).default,
    ).toBe("replacement bytes");
    await replacement.close();
  });

  it("captures the lease bytes before asynchronous hashing can mutate their source", async () => {
    const bytes = new TextEncoder().encode(
      "export default 'captured before hash'",
    );
    const entry = {
      ...unit(),
      module: { path: "mutable.js", digest: hash(bytes) },
    };
    const digest = crypto.subtle.digest.bind(crypto.subtle);
    const hashBytes = vi
      .spyOn(crypto.subtle, "digest")
      .mockImplementation(async (algorithm, captured) => {
        bytes.fill(0);
        return digest(algorithm, captured);
      });
    const owner = new Lifetime("captured bytes");
    const modules = new RealmModules(async () => ({ bytes, close() {} }));
    try {
      expect((await modules.evaluate(entry, owner, plan(entry))).default).toBe(
        "captured before hash",
      );
    } finally {
      hashBytes.mockRestore();
      await owner.close();
    }
  });

  it("shares the native identity across live artifact aliases and refuses an alias after retirement", async () => {
    const modules = new RealmModules(async () => ({
      bytes: source,
      close() {},
    }));
    const first = unit("first.js");
    const second = {
      ...unit("second.js"),
      instance: {
        ...first.instance,
        source: { ...first.instance.source, installationId: "alias" },
      },
    };
    const a = new Lifetime("first alias"),
      b = new Lifetime("second alias");
    expect(await modules.evaluate(first, a, plan(first))).toBe(
      await modules.evaluate(second, b, plan(second)),
    );
    await a.close();
    const retained = new Lifetime("retained alias");
    expect(await modules.evaluate(first, retained, plan(first))).toBe(
      await modules.evaluate(second, b, plan(second)),
    );
    await retained.close();
    await b.close();
    const later = new Lifetime("retired alias");
    const third = {
      ...unit("third.js"),
      instance: {
        ...first.instance,
        source: { ...first.instance.source, installationId: "third" },
      },
    };
    await expect(
      modules.evaluate(third, later, plan(third)),
    ).rejects.toMatchObject({ code: "reload_required" });
    await later.close();
  });

  it("refuses bytes that do not match the Host-approved digest", async () => {
    const close = vi.fn();
    const modules = new RealmModules(async () => ({
      bytes: new TextEncoder().encode("export default 'unapproved'"),
      close,
    }));
    const owner = new Lifetime("plaintext");
    const entry = unit();
    await expect(modules.load(entry, owner, plan(entry))).rejects.toMatchObject(
      {
        code: "contract_mismatch",
      },
    );
    await owner.close();
    expect(close).toHaveBeenCalledOnce();
  });

  it("evaluates once and retires only after every actual lease releases", async () => {
    const close = vi.fn();
    const modules = new RealmModules(async () => ({
      bytes: source,
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
    const open = vi.fn(async () => ({ bytes: source, close() {} }));
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
      bytes: source,
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
    const bytes = new TextEncoder().encode(
      `globalThis["${key}"].start(); await globalThis["${key}"].resume; export default {id:'preview',imports:{},exports:{},setup(){return {}}}`,
    );
    const url =
      "data:text/javascript;base64," + Buffer.from(bytes).toString("base64");
    const close = vi.fn();
    const modules = new RealmModules(async () => ({ bytes, close }));
    const owner = new Lifetime("evaluating");
    const entry = {
      ...unit(),
      module: { path: "pending.js", digest: hash(bytes) },
    };
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
      bytes: Uint8Array<ArrayBuffer>;
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
    pending.resolve({ bytes: source, close });
    expect((await retiring).status).toBe("released");
    expect(close).toHaveBeenCalledOnce();
    expect(await loading).toMatchObject({ code: "closed" });
  });
});

it("evaluates each exact installation and artifact even when their slash paths collide", async () => {
  const first = unit("b/c.js"),
    second = unit("c.js");
  const entries = [first, second].map((entry, index) => ({
    ...entry,
    instance: {
      ...entry.instance,
      source: {
        ...entry.instance.source,
        installationId: ["a", "a/b"][index]!,
      },
    },
    module: {
      ...entry.module,
      digest: hash(
        new TextEncoder().encode(
          "export default " + JSON.stringify(entry.module.path),
        ),
      ),
    },
  }));
  const modules = new RealmModules(async (entry) => ({
    bytes: new TextEncoder().encode(
      "export default " + JSON.stringify(entry.module.path),
    ),
    close() {},
  }));
  const owner = new Lifetime("cache");
  for (const entry of entries)
    expect((await modules.evaluate(entry, owner, plan(entry))).default).toBe(
      entry.module.path,
    );
  await owner.close();
});
