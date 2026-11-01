import { expect, it } from "vitest";
import { Service } from "./contracts";
import { defineUnit } from "./unit";
import { MAX_NAME_BYTES, type ContractRef } from "./protocol.generated";

const reference: ContractRef = {
  name: "example.value",
  version: "1",
  digest: "a".repeat(64),
  kind: "service",
  mode: "native",
};

it.each([
  "",
  "two words",
  "line\nbreak",
  "control\u0000character",
  "\ud800",
  "a".repeat(MAX_NAME_BYTES + 1),
  "界".repeat(Math.floor(MAX_NAME_BYTES / 3) + 1),
])("rejects names outside the Go owner's profile: %j", (name) => {
  expect(() => new Service({ ...reference, name })).toThrow();
  expect(() => new Service({ ...reference, version: name })).toThrow();
  const service = new Service<number>(reference);
  expect(() =>
    defineUnit({ id: name, imports: {}, exports: {}, setup: () => ({}) }),
  ).toThrow();
  expect(() =>
    defineUnit({
      id: "unit",
      imports: { [name]: service },
      exports: {},
      setup: () => ({}),
    }),
  ).toThrow();
  expect(() =>
    defineUnit({
      id: "unit",
      imports: {},
      exports: { [name]: service },
      setup: () => ({ [name]: 1 }),
    }),
  ).toThrow();
});

it.each([
  "a".repeat(MAX_NAME_BYTES),
  "界".repeat(Math.floor(MAX_NAME_BYTES / 3)),
  "\ufeffcontract",
  "😀",
])("preserves admitted Unicode identities: %j", (name) => {
  expect(new Service({ ...reference, name }).ref.name).toBe(name);
  expect(
    defineUnit({ id: name, imports: {}, exports: {}, setup: () => ({}) }).id,
  ).toBe(name);
});
