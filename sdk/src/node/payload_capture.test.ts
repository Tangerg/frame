import { PassThrough } from "node:stream";
import { expect, it, onTestFinished } from "vitest";
import { Capability, defineProcessUnit, serve, type Client } from "./index.js";
import { Peer } from "./peer.js";
import { PROTOCOL } from "./contract.js";
import wire from "./wire.json" with { type: "json" };

const bounded = {
  type: "object",
  properties: { value: { type: "integer", maximum: 1 } },
  required: ["value"],
  additionalProperties: false,
} as const;
const contract = new Capability({
  protocol: PROTOCOL,
  reference: "capture.capture",
  methods: [
    {
      name: "read",
      kind: "query",
      description: "",
      input: bounded,
      output: bounded,
    },
  ],
});

function changingValue() {
  let reads = 0;
  return {
    value: {
      get value() {
        return ++reads;
      },
    },
    reads: () => reads,
  };
}

it("returns the output value admitted by the method boundary", async () => {
  const input = new PassThrough(),
    output = new PassThrough();
  const frames: string[] = [];
  output.on("data", (frame: Buffer) => frames.push(frame.toString()));
  const result = changingValue();
  const serving = serve(
    defineProcessUnit({
      id: "capture-provider",
      imports: {},
      exports: { value: contract },
      setup: () => ({ value: { read: () => result.value } }),
    }),
    { input, output },
  );
  const host = new Peer(output, input);
  const running = host.run(async () => true);
  onTestFinished(async () => {
    host.stop(new Error("session completed"));
    await Promise.allSettled([serving, running]);
  });
  const signal = new AbortController().signal;
  await host.request(
    wire.methods.hello,
    { version: wire.version, maxFrameBytes: wire.maxFrameBytes },
    signal,
  );
  await host.request(wire.methods.prepare, { imports: {} }, signal);
  await host.request(wire.methods.activate, {}, signal);
  const actual = await host.request(
    wire.methods.call,
    { handle: "value", member: "read", input: { value: 1 } },
    signal,
  );
  expect({ actual, reads: result.reads(), frame: frames.at(-1) }).toEqual({
    actual: { value: 1 },
    reads: 1,
    frame: '{"id":"4","result":{"value":1}}\n',
  });
});

it("sends the input value admitted by the method boundary", async () => {
  const input = new PassThrough(),
    output = new PassThrough();
  const frames: string[] = [];
  output.on("data", (frame: Buffer) => frames.push(frame.toString()));
  let client!: Client<typeof contract>;
  const serving = serve(
    defineProcessUnit({
      id: "capture-consumer",
      imports: { value: contract },
      exports: {},
      setup({ imports }) {
        client = imports.value;
        return {};
      },
    }),
    { input, output },
  );
  let actual: unknown;
  const host = new Peer(output, input);
  const running = host.run(async (_method, params) => {
    actual = (params as { input: unknown }).input;
    return { value: 1 };
  });
  onTestFinished(async () => {
    host.stop(new Error("session completed"));
    await Promise.allSettled([serving, running]);
  });
  const signal = new AbortController().signal;
  await host.request(
    wire.methods.hello,
    { version: wire.version, maxFrameBytes: wire.maxFrameBytes },
    signal,
  );
  await host.request(
    wire.methods.prepare,
    {
      imports: {
        value: [
          {
            handle: "host-value",
            source: {
              installationId: "host",
              release: "a".repeat(64),
              unitId: "host",
              export: "value",
              contract: {
                name: contract.descriptor.reference,
                version: "1",
                digest: contract.identity,
                mode: "portable",
                kind: "service",
              },
            },
          },
        ],
      },
    },
    signal,
  );
  await host.request(wire.methods.activate, {}, signal);
  const argument = changingValue();
  await client.read(argument.value);
  expect({ actual, reads: argument.reads(), frame: frames.at(-1) }).toEqual({
    actual: { value: 1 },
    reads: 1,
    frame:
      '{"id":"1","method":"call","params":{"handle":"host-value","member":"read","input":{"value":1}}}\n',
  });
});

it("emits the chunk value admitted by the method boundary", async () => {
  const stream = new Capability({
    protocol: PROTOCOL,
    reference: "capture.stream",
    methods: [
      {
        name: "read",
        kind: "query",
        description: "",
        input: { type: "integer" },
        output: { type: "integer" },
        stream: bounded,
      },
    ],
  });
  const input = new PassThrough(),
    output = new PassThrough();
  const frames: string[] = [];
  output.on("data", (frame: Buffer) => frames.push(frame.toString()));
  const chunk = changingValue();
  const serving = serve(
    defineProcessUnit({
      id: "capture-stream",
      imports: {},
      exports: { value: stream },
      setup: () => ({
        value: stream.implement({
          async read(value, context) {
            await context.emit(chunk.value);
            return value;
          },
        }),
      }),
    }),
    { input, output },
  );
  let actual: unknown;
  const host = new Peer(output, input);
  const running = host.run(async (_method, params) => {
    actual = (params as { value: unknown }).value;
    return true;
  });
  onTestFinished(async () => {
    host.stop(new Error("session completed"));
    await Promise.allSettled([serving, running]);
  });
  const signal = new AbortController().signal;
  await host.request(
    wire.methods.hello,
    { version: wire.version, maxFrameBytes: wire.maxFrameBytes },
    signal,
  );
  await host.request(wire.methods.prepare, { imports: {} }, signal);
  await host.request(wire.methods.activate, {}, signal);
  await host.request(
    wire.methods.call,
    {
      handle: "value",
      member: "read",
      input: 1,
      stream: "capture-stream-handle",
    },
    signal,
  );
  expect({
    actual,
    reads: chunk.reads(),
    frame: frames.find((frame) => frame.includes('"method":"chunk"')),
  }).toEqual({
    actual: { value: 1 },
    reads: 1,
    frame:
      '{"id":"1","method":"chunk","params":{"stream":"capture-stream-handle","value":{"value":1}}}\n',
  });
});
