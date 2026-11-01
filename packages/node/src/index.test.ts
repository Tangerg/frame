import { PassThrough } from "node:stream";
import { createInterface } from "node:readline";
import { describe, it, expect } from "vitest";
import { Capability, defineProcessUnit, serve } from "./index.js";
import { PROTOCOL } from "./contract.js";
import wire from "./wire.json" with { type: "json" };

const numbers = new Capability({
  protocol: PROTOCOL,
  reference: "math",
  methods: [
    {
      name: "count",
      kind: "query",
      description: "",
      input: { type: "integer" },
      output: { type: "integer" },
      stream: { type: "integer" },
    },
  ],
});

describe("local process author", () => {
  it("pins the export contract, streams with acknowledgement and joins shutdown", async () => {
    const input = new PassThrough(),
      output = new PassThrough();
    const responses = createInterface({ input: output })[
      Symbol.asyncIterator
    ]();
    const unit = defineProcessUnit({
      id: "counter",
      imports: {},
      exports: { numbers },
      setup() {
        return {
          numbers: numbers.implement({
            async count(value, ctx) {
              await ctx.emit(value);
              return value;
            },
          }),
        };
      },
    });
    const run = serve(unit, { input, output });
    let id = 0;
    const send = (value: unknown) => input.write(JSON.stringify(value) + "\n");
    const request = (method: string, params: unknown) =>
      send({ id: String(++id), method, params });
    const next = async () => JSON.parse((await responses.next()).value!);
    request("hello", { version: wire.version, maxFrameBytes: 8192 });
    expect(await next()).toMatchObject({
      id: "1",
      result: { exports: { numbers: numbers.identity }, unit: "counter" },
    });
    request("activate", {});
    expect(await next()).toMatchObject({ error: { code: wire.errors.closed } });
    request("prepare", { imports: {} });
    expect(await next()).toMatchObject({ result: true });
    request("activate", {});
    expect(await next()).toMatchObject({ result: true });
    request("call", {
      handle: "numbers",
      member: "count",
      stream: "s",
      input: 7,
    });
    const chunk = await next();
    expect(chunk).toMatchObject({
      method: "chunk",
      params: { stream: "s", value: 7 },
    });
    send({ id: chunk.id, result: true });
    expect(await next()).toMatchObject({ id: "5", result: 7 });
    request("close", {});
    expect(await next()).toMatchObject({ result: true });
    input.end();
    await run;
  });
  it("refuses an oversized unterminated frame without waiting for EOF", async () => {
    const input = new PassThrough(),
      output = new PassThrough();
    const run = serve(
      defineProcessUnit({
        id: "empty",
        imports: {},
        exports: {},
        setup: () => ({}),
      }),
      { input, output },
    );
    input.write(Buffer.alloc(wire.maxFrameBytes, 32));
    await expect(run).rejects.toThrow("frame too large");
  });
});

describe("process outcome evidence", () => {
  it("distinguishes cancellation before dispatch and an interrupted original call", async () => {
    const { Peer } = await import("./peer.js");
    const input = new PassThrough(),
      output = new PassThrough(),
      peer = new Peer(input, output);
    const reason = new Error("stopped");
    expect(() => peer.request("call", {}, AbortSignal.abort(reason))).toThrow(
      expect.objectContaining({ code: wire.errors.cancelled, cause: reason }),
    );
    const controller = new AbortController();
    const original = peer.request("call", {}, controller.signal);
    controller.abort(reason);
    await expect(original).rejects.toMatchObject({
      code: wire.errors.unknown,
      cause: reason,
    });
    const second = peer.request("call", {}, new AbortController().signal);
    peer.stop(reason);
    await expect(second).rejects.toMatchObject({
      code: wire.errors.unknown,
      cause: reason,
    });
  });

  it("refuses an excess concurrent request while preserving the live session", async () => {
    const { Peer } = await import("./peer.js");
    const input = new PassThrough(),
      output = new PassThrough(),
      peer = new Peer(input, output);
    const lines = createInterface({ input: output })[Symbol.asyncIterator]();
    let release!: () => void;
    const wait = new Promise<void>((done) => {
      release = done;
    });
    const run = peer.run(async () => {
      await wait;
      return true;
    });
    for (let id = 1; id <= wire.maxInFlight + 1; id++)
      input.write(
        JSON.stringify({ id: String(id), method: "test", params: {} }) + "\n",
      );
    const refused = JSON.parse((await lines.next()).value!);
    expect(refused).toMatchObject({
      id: String(wire.maxInFlight + 1),
      error: { code: wire.errors.capacity },
    });
    release();
    for (let i = 0; i < wire.maxInFlight; i++)
      expect(JSON.parse((await lines.next()).value!).result).toBe(true);
    input.write(
      JSON.stringify({
        id: String(wire.maxInFlight + 2),
        method: "test",
        params: {},
      }) + "\n",
    );
    expect(JSON.parse((await lines.next()).value!).result).toBe(true);
    input.end();
    await run;
  });
});
