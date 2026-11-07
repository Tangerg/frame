import { expect, it } from "vitest";
import { Service } from "./contracts.js";
import { Lifetime } from "./lifetime.js";
import { defineUnit, type Setup } from "./unit.js";

it("captures setup once and keeps the class instance that owns its private state", async () => {
  const answer = new Service<number>({
    name: "example.answer",
    version: "1",
    digest: "a".repeat(64),
    mode: "native",
    kind: "service",
  });
  class Definition {
    readonly id = "stateful";
    readonly imports = {};
    readonly exports = { answer };
    readonly #answer = 40;
    setupReads = 0;

    get setup() {
      this.setupReads++;
      return this.#setup;
    }

    #setup({ configuration }: Setup<Record<string, never>>) {
      return { answer: this.#answer + Number(configuration) };
    }
  }
  const source = new Definition();
  const unit = defineUnit(source);
  Object.defineProperty(source, "setup", {
    value: () => {
      throw new Error("replacement setup must not run");
    },
  });
  const lifetime = new Lifetime("captured setup");
  try {
    const setup = unit.setup;
    expect(await setup({ imports: {}, configuration: 2, lifetime })).toEqual({
      answer: 42,
    });
    expect(source.setupReads).toBe(1);
  } finally {
    await lifetime.close();
  }
});

it("retains each declaration field and slot from its validated capture", () => {
  const service = new Service<number>({
    name: "example.value",
    version: "1",
    digest: "a".repeat(64),
    mode: "native",
    kind: "service",
  });
  let idReads = 0;
  let importsReads = 0;
  let exportsReads = 0;
  let setupReads = 0;
  const sourceImports = { input: service };
  const sourceExports = { output: service };
  const setup = ({ imports }: Setup<typeof sourceImports>) => ({
    output: imports.input,
  });
  const unit = defineUnit({
    get id() {
      return ++idReads === 1 ? "captured" : "";
    },
    get imports() {
      importsReads++;
      return sourceImports;
    },
    get exports() {
      exportsReads++;
      return sourceExports;
    },
    get setup() {
      setupReads++;
      return setup;
    },
  });
  expect(unit).toEqual({
    id: "captured",
    imports: sourceImports,
    exports: sourceExports,
    setup: expect.any(Function),
  });
  expect({ idReads, importsReads, exportsReads, setupReads }).toEqual({
    idReads: 1,
    importsReads: 1,
    exportsReads: 1,
    setupReads: 1,
  });
  expect(unit.imports).not.toBe(sourceImports);
  expect(unit.exports).not.toBe(sourceExports);
  expect(Object.isFrozen(unit.imports)).toBe(true);
  expect(Object.isFrozen(unit.exports)).toBe(true);
});

it("captures optional import fields without copying their immutable contract", () => {
  const service = new Service<number>({
    name: "example.value",
    version: "1",
    digest: "a".repeat(64),
    mode: "native",
    kind: "service",
  });
  let contractReads = 0;
  let optionalReads = 0;
  const declaration = {
    get contract() {
      contractReads++;
      return service;
    },
    get optional(): true {
      optionalReads++;
      return true;
    },
  };
  const unit = defineUnit({
    id: "consumer",
    imports: { optional: declaration },
    exports: {},
    setup: () => ({}),
  });
  Object.defineProperty(declaration, "contract", { value: undefined });
  Object.defineProperty(declaration, "optional", { value: false });
  expect(unit.imports.optional).toEqual({ contract: service, optional: true });
  expect(unit.imports.optional.contract).toBe(service);
  expect({ contractReads, optionalReads }).toEqual({
    contractReads: 1,
    optionalReads: 1,
  });
  expect(Object.isFrozen(unit.imports.optional)).toBe(true);
});
