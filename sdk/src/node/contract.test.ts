import { expect, it } from "vitest";
import { ExactNumber } from "../json.js";
import { Capability, PROTOCOL, type Client } from "./contract.js";

const shapes = new Capability({
  protocol: PROTOCOL,
  reference: "projection.shapes",
  methods: [
    {
      name: "open",
      kind: "query",
      description: "",
      input: {
        type: "object",
        properties: { title: { type: "string" } },
        required: ["title"],
      },
      output: { type: "boolean" },
    },
    {
      name: "closed",
      kind: "query",
      description: "",
      input: { type: "object", additionalProperties: false },
      output: { type: "boolean" },
    },
    {
      name: "typed",
      kind: "query",
      description: "",
      input: {
        type: "object",
        properties: { title: { type: "string" } },
        additionalProperties: { type: "number" },
        required: ["title", "amount"],
      },
      output: { type: "boolean" },
    },
    {
      name: "constant",
      kind: "query",
      description: "",
      input: { const: 1 },
      output: { type: "boolean" },
    },
    {
      name: "enumerated",
      kind: "query",
      description: "",
      input: { enum: [1, "empty"] },
      output: { type: "boolean" },
    },
    {
      name: "nested",
      kind: "query",
      description: "",
      input: { const: { items: [1], empty: {} } },
      output: { type: "boolean" },
    },
    {
      name: "stream",
      kind: "query",
      description: "",
      input: { type: "boolean" },
      output: { type: "boolean" },
      stream: { type: "number" },
    },
    {
      name: "command",
      kind: "command",
      description: "",
      input: { type: "boolean" },
      output: { type: "boolean" },
    },
    {
      name: "streamCommand",
      kind: "command",
      description: "",
      input: { type: "boolean" },
      output: { type: "boolean" },
      stream: { type: "number" },
    },
  ],
});

function checkClientProjection(client: Client<typeof shapes>): void {
  void client.open({ title: "kept", extension: true });
  // @ts-expect-error Required members remain required on an open object.
  void client.open({ extension: true });
  void client.closed({});
  // @ts-expect-error A closed empty object cannot contain another member.
  void client.closed({ extension: true });
  // @ts-expect-error TypeScript's unconstrained empty object type is unsound here.
  void client.closed("not an object");
  void client.typed({ title: "kept", amount: 1n, extra: 2 });
  // @ts-expect-error Required names outside properties use the additional schema.
  void client.typed({ title: "kept" });
  // @ts-expect-error Typed additional members cannot contain booleans.
  void client.typed({ title: "kept", amount: 1, extra: true });
  void client.constant(1n);
  void client.constant(new ExactNumber("1.0"));
  // @ts-expect-error Numeric equality does not admit strings.
  void client.constant("1");
  void client.enumerated(new ExactNumber("1e0"));
  void client.enumerated("empty");
  // @ts-expect-error String enum members retain their literal identity.
  void client.enumerated("other");
  void client.nested({ items: [1n], empty: {} });
  // @ts-expect-error Exact object literals retain their member structure.
  void client.nested({ items: [1], empty: "not an object" });
  void client.stream(true, { onChunk() {} });
  // @ts-expect-error A streaming query requires a consumer before dispatch.
  void client.stream(true);
  // @ts-expect-error Supplying other options does not establish a consumer.
  void client.stream(true, { signal: new AbortController().signal });
  // @ts-expect-error Unary methods cannot accept a stream consumer.
  void client.closed({}, { onChunk() {} });
  // @ts-expect-error Queries do not carry business command identities.
  void client.stream(true, { commandID: "command", onChunk() {} });
  void client.command(true, { commandID: "command" });
  // @ts-expect-error Unary commands retain their required business identity.
  void client.command(true);
  void client.streamCommand(true, { commandID: "command", onChunk() {} });
  // @ts-expect-error A streaming command requires its consumer and identity.
  void client.streamCommand(true, { commandID: "command" });
  // @ts-expect-error A consumer cannot substitute for a command identity.
  void client.streamCommand(true, { onChunk() {} });
}
void checkClientProjection;

function checkDynamicDescriptor(client: Client<Capability>): void {
  void client["loaded"]!(true, {
    commandID: "original-command",
    onChunk: (value: unknown) => {
      void value;
    },
  });
}
void checkDynamicDescriptor;

it("admits the exact values represented by the inferred structural types", () => {
  for (const [name, input] of [
    ["open", { title: "kept", extension: true }],
    ["closed", {}],
    ["typed", { title: "kept", amount: 1n, extra: 2 }],
    ["constant", 1n],
    ["constant", new ExactNumber("1.0")],
    ["enumerated", new ExactNumber("1e0")],
    ["nested", { items: [1n], empty: {} }],
  ] as const)
    expect(() => shapes.method(name).admitInput(input)).not.toThrow();

  for (const [name, input] of [
    ["open", { extension: true }],
    ["closed", { extension: true }],
    ["closed", "not an object"],
    ["typed", { title: "kept" }],
    ["typed", { title: "kept", amount: 1, extra: true }],
    ["constant", "1"],
    ["enumerated", "other"],
    ["nested", { items: [1], empty: "not an object" }],
  ] as const)
    expect(() => shapes.method(name).admitInput(input)).toThrow();
});
