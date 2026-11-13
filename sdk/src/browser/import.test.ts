import { expect, it, vi } from "vitest";
import { defineUnit, optional, Service, type Setup } from "../index.js";
import type { Import } from "../protocol.generated.js";
import { Runtime, type RuntimeOptions } from "./runtime.js";
import {
  authority,
  digest,
  fixture,
  plan,
  realm,
  ref,
  unit,
} from "./runtime.test.fixture.js";

const files = new Service<unknown>({
  ...ref("test.remote-files"),
  mode: "portable",
});

function remoteImport(): Import {
  return {
    slot: "files",
    contract: files.ref,
    kind: "remote",
    remote: {
      source: {
        installationId: "files",
        release: digest,
        unitId: "backend",
        export: "files",
        contract: files.ref,
      },
      token: "original-route",
    },
  };
}

function importFixture(config: {
  declaration: "required" | "optional";
  implementation: unknown;
  binding: Import;
}) {
  const slots = {
    files: config.declaration === "optional" ? optional(files) : files,
  };
  const setup = vi.fn((_context: Setup<typeof slots>) => ({}));
  const close = vi.fn();
  const definition = defineUnit({
    id: "consumer",
    imports: slots,
    exports: {},
    setup,
  });
  const { control } = fixture({});
  const remote = vi.fn<RuntimeOptions["remote"]>((_binding, owner) => {
    owner.cleanup(close, "remote client");
    return config.implementation as NonNullable<unknown>;
  });
  const runtime = new Runtime({
    authority,
    realm,
    control,
    proofCapacity: 8,
    load: () => definition,
    remote,
  });
  const approved = plan("consumer", [
    unit("consumer", "consumer", [], [config.binding]),
  ]);
  return { runtime, control, remote, setup, close, approved };
}

for (const declaration of ["required", "optional"] as const) {
  it.each([null, undefined])(
    `rejects a missing remote implementation before setup for a ${declaration} slot: %s`,
    async (implementation) => {
      const f = importFixture({
        declaration,
        implementation,
        binding: remoteImport(),
      });
      try {
        await expect(f.runtime.prepare(f.approved)).rejects.toMatchObject({
          code: "contract_mismatch",
        });
        expect(f.setup).not.toHaveBeenCalled();
        expect(f.control.ready).not.toHaveBeenCalled();
        expect(f.close).toHaveBeenCalledOnce();
      } finally {
        await f.runtime.close();
      }
    },
  );
}

it.each([0, false])(
  "preserves the selected non-null remote implementation: %s",
  async (implementation) => {
    const f = importFixture({
      declaration: "required",
      implementation,
      binding: remoteImport(),
    });
    try {
      await f.runtime.prepare(f.approved);
      expect(f.setup).toHaveBeenCalledOnce();
      expect(f.setup.mock.calls[0]![0].imports.files).toBe(implementation);
    } finally {
      await f.runtime.close();
    }
  },
);

it("uses undefined only when the Host admitted an optional absence", async () => {
  const f = importFixture({
    declaration: "optional",
    implementation: undefined,
    binding: { slot: "files", contract: files.ref, kind: "absent" },
  });
  try {
    await f.runtime.prepare(f.approved);
    expect(f.remote).not.toHaveBeenCalled();
    expect(f.setup.mock.calls[0]![0].imports.files).toBeUndefined();
  } finally {
    await f.runtime.close();
  }
});
