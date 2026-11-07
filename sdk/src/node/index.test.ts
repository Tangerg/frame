import { PassThrough, Writable } from "node:stream";
import { createInterface } from "node:readline";
import { describe, it, expect, onTestFinished } from "vitest";
import {
  Capability,
  defineProcessUnit,
  serve,
  ProcessError,
  type Client,
  type RequestOptions,
  type ProcessUnit,
} from "./index.js";
import { PROTOCOL, exactFields, type ServiceContract } from "./contract.js";
import { Peer } from "./peer.js";
import { PORTABLE_LIMITS } from "./limits.js";
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

const diagnosticFailures = [
  {
    name: "Error.name",
    fail() {
      return Object.defineProperty(new Error("consumer failed"), "name", {
        get() {
          throw new Error("diagnostic getter failed");
        },
      });
    },
  },
  {
    name: "ProcessError.code",
    fail() {
      return new Proxy(new ProcessError(wire.errors.unknown, "failed"), {
        get() {
          throw new Error("diagnostic getter failed");
        },
      });
    },
  },
  {
    name: "a throwing prototype",
    fail() {
      return new Proxy(
        {},
        {
          getPrototypeOf() {
            throw new Error("diagnostic prototype failed");
          },
        },
      );
    },
  },
];

describe("local process author", () => {
  it("keeps a captured setup method on its original class instance", async () => {
    class Unit implements ProcessUnit<{}, {}> {
      id = "class-unit";
      imports = {};
      exports = {};
      #prepared = 0;

      setup() {
        this.#prepared++;
        return {};
      }

      get prepared() {
        return this.#prepared;
      }
    }
    const source = new Unit();
    const captured = defineProcessUnit(source);
    source.setup = () => {
      throw new Error("replacement setup must not run");
    };
    await connectUnit(captured);
    expect(source.prepared).toBe(1);
  });

  it("validates and retains one capture of a unit and its import cardinality", () => {
    let idReads = 0;
    let importReads = 0;
    let contractReads = 0;
    let cardinalityReads = 0;
    const declaration = {
      get contract() {
        contractReads++;
        return numbers;
      },
      get cardinality(): "optional" | "many" {
        return ++cardinalityReads === 1 ? "optional" : "many";
      },
    };
    const unit = defineProcessUnit({
      get id() {
        return ++idReads === 1 ? "consumer" : "";
      },
      get imports() {
        importReads++;
        return { numbers: declaration };
      },
      exports: {},
      setup: () => ({}),
    });
    expect(unit.id).toBe("consumer");
    expect(unit.imports.numbers).toEqual({
      contract: numbers,
      cardinality: "optional",
    });
    expect({ idReads, importReads, contractReads, cardinalityReads }).toEqual({
      idReads: 1,
      importReads: 1,
      contractReads: 1,
      cardinalityReads: 1,
    });
    expect(Object.isFrozen(unit.imports.numbers)).toBe(true);
  });

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
  it("retires after peer EOF even when a response cannot drain", async () => {
    const { Peer } = await import("./peer.js");
    const input = new PassThrough();
    let writing!: () => void;
    const started = new Promise<void>((resolve) => {
      writing = resolve;
    });
    const output = new Writable({
      write() {
        writing();
      },
    });
    const peer = new Peer(input, output);
    const run = peer.run(async () => true);
    input.write('{"id":"1","method":"test","params":{}}\n');
    await started;
    input.end();
    await run;
    expect(output.destroyed).toBe(true);
  });

  it("distinguishes cancellation before dispatch and an interrupted original call", async () => {
    const { Peer } = await import("./peer.js");
    const input = new PassThrough(),
      output = new PassThrough(),
      peer = new Peer(input, output);
    const reason = new Error("stopped");
    expect(() => peer.request("call", {}, AbortSignal.abort(reason))).toThrow(
      expect.objectContaining({
        code: wire.errors.cancelled,
        cause: reason,
        outcomeUnknown: false,
      }),
    );
    const controller = new AbortController();
    const original = peer.request("call", {}, controller.signal);
    controller.abort(reason);
    await expect(original).rejects.toMatchObject({
      code: wire.errors.cancelled,
      cause: reason,
      outcomeUnknown: true,
    });
    const second = peer.request("call", {}, new AbortController().signal);
    peer.stop(reason);
    await expect(second).rejects.toMatchObject({
      code: wire.errors.unknown,
      cause: reason,
      outcomeUnknown: true,
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

async function connectUnit<
  I extends Record<string, ServiceContract>,
  E extends Record<string, ServiceContract>,
>(
  unit: ProcessUnit<I, E>,
  imports: unknown = {},
  dispatch: (
    peer: Peer,
    method: string,
    params: unknown,
    signal: AbortSignal,
  ) => Promise<unknown> = async () => true,
): Promise<Peer> {
  const input = new PassThrough();
  const output = new PassThrough();
  const serving = serve(unit, { input, output });
  const host = new Peer(output, input);
  const running = host.run((method, params, signal) =>
    dispatch(host, method, params, signal),
  );
  onTestFinished(async () => {
    host.stop(new Error("test completed"));
    // Both peers own their streams; explicitly stopping one interrupts the
    // other's async iterator after the assertions have completed.
    for (const result of await Promise.allSettled([serving, running])) {
      if (result.status === "rejected")
        expect(result.reason).toMatchObject({
          code: "ERR_STREAM_PREMATURE_CLOSE",
        });
    }
  });
  await host.request(
    wire.methods.hello,
    {
      version: wire.version,
      maxFrameBytes: wire.maxFrameBytes,
    },
    new AbortController().signal,
  );
  await host.request(
    wire.methods.prepare,
    { imports },
    new AbortController().signal,
  );
  await host.request(wire.methods.activate, {}, new AbortController().signal);
  return host;
}

it("prepares callable handlers from one private capture of the exported service tables", async () => {
  let serviceReads = 0;
  let handlerReads = 0;
  let calls = 0;
  const original = () => {
    calls++;
    return 7;
  };
  const members = {
    get count() {
      return ++handlerReads === 1 ? original : () => 99;
    },
  };
  const handlers = {
    get numbers() {
      serviceReads++;
      return members;
    },
  };
  const peer = await connectUnit(
    defineProcessUnit({
      id: "captured-handlers",
      imports: {},
      exports: { numbers },
      setup: () => handlers,
    }),
  );
  Object.defineProperty(members, "count", { value: () => 99 });
  Object.defineProperty(handlers, "numbers", { value: { count: () => 55 } });
  expect(
    await peer.request(
      wire.methods.call,
      {
        handle: "numbers",
        member: "count",
        input: 1,
        stream: "original",
      },
      new AbortController().signal,
    ),
  ).toBe(7);
  expect({ serviceReads, handlerReads, calls }).toEqual({
    serviceReads: 1,
    handlerReads: 1,
    calls: 1,
  });
});

function serviceBinding(slot: string, contract: ServiceContract) {
  return {
    [slot]: [
      {
        handle: `host-${slot}`,
        source: {
          installationId: "host",
          release: "a".repeat(64),
          unitId: "host-unit",
          export: slot,
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
  };
}

it("joins an imported stream's local consumer before unit cleanup", async () => {
  const started = Promise.withResolvers<void>();
  const finish = Promise.withResolvers<void>();
  let client!: Client<typeof numbers>;
  let cleaned = false;
  const peer = await connectUnit(
    defineProcessUnit({
      id: "consumer",
      imports: { numbers },
      exports: {},
      setup({ imports, lifetime }) {
        client = imports.numbers;
        lifetime.cleanup(() => {
          cleaned = true;
        });
        return {};
      },
    }),
    serviceBinding("numbers", numbers),
    async (host, method, params, signal) => {
      expect(method).toBe(wire.methods.call);
      exactFields(params, ["handle", "member", "input", "stream"]);
      await host.request(
        wire.methods.chunk,
        { stream: params.stream, value: params.input },
        signal,
      );
      return params.input;
    },
  );
  const calling = client.count(1, {
    onChunk: async () => {
      started.resolve();
      await finish.promise;
    },
  });
  const cancelled = expect(calling).rejects.toMatchObject({
    outcomeUnknown: true,
  });
  await started.promise;
  let closed = false;
  const closing = peer
    .request(wire.methods.close, {}, new AbortController().signal)
    .then((result) => {
      closed = true;
      return result;
    });
  try {
    await new Promise<void>((resolve) => setImmediate(resolve));
    expect(cleaned).toBe(false);
    expect(closed).toBe(false);
  } finally {
    finish.resolve();
    await cancelled;
    expect(await closing).toBe(true);
  }
  expect(cleaned).toBe(true);
});

describe("query and command invocation identity", () => {
  const operations = new Capability({
    protocol: PROTOCOL,
    reference: "operations",
    methods: [
      {
        name: "read",
        kind: "query",
        description: "",
        input: { type: "integer" },
        output: { type: "integer" },
      },
      {
        name: "change",
        kind: "command",
        description: "",
        input: { type: "integer" },
        output: { type: "integer" },
      },
      {
        name: "changeStream",
        kind: "command",
        description: "",
        input: { type: "integer" },
        output: { type: "integer" },
        stream: { type: "integer" },
      },
    ],
  });
  const identities = [
    " original / operation ",
    "😀".repeat(PORTABLE_LIMITS.maxCommandIDCharacters),
  ];

  it("forwards caller identities through imported unary and streaming commands", async () => {
    let client!: Client<typeof operations>;
    const requests: Record<string, unknown>[] = [];
    await connectUnit(
      defineProcessUnit({
        id: "caller",
        imports: { operations },
        exports: {},
        setup({ imports }) {
          client = imports.operations;
          return {};
        },
      }),
      serviceBinding("operations", operations),
      async (peer, method, params, signal) => {
        expect(method).toBe(wire.methods.call);
        exactFields(
          params,
          ["handle", "member", "input"],
          ["stream", "commandID"],
        );
        requests.push(params);
        if (params.stream)
          await peer.request(
            wire.methods.chunk,
            { stream: params.stream, value: params.input },
            signal,
          );
        return params.input;
      },
    );
    const unchecked = client as unknown as Record<
      string,
      (input: unknown, options?: RequestOptions) => Promise<unknown>
    >;
    for (const commandID of [
      undefined,
      null,
      "",
      "a".repeat(PORTABLE_LIMITS.maxCommandIDCharacters + 1),
      "\ud800",
    ]) {
      await expect(
        unchecked.change!(1, { commandID: commandID as string }),
      ).rejects.toThrow("command identity");
    }
    for (const commandID of [null, "", "not-a-query"]) {
      await expect(
        unchecked.read!(1, { commandID: commandID as string }),
      ).rejects.toThrow("query must not carry");
    }
    expect(requests).toEqual([]);
    expect(await client.read(1)).toBe(1);
    expect(Object.hasOwn(requests[0]!, "commandID")).toBe(false);
    const chunks: number[] = [];
    for (const commandID of identities) {
      expect(await client.change(2, { commandID })).toBe(2);
      expect(
        await client.changeStream(3, {
          commandID,
          onChunk(value) {
            chunks.push(value as number);
          },
        }),
      ).toBe(3);
    }
    expect(requests.slice(1).map((request) => request.commandID)).toEqual(
      identities.flatMap((identity) => [identity, identity]),
    );
    expect(chunks).toEqual([3, 3]);
  });

  it("validates invocation kind before entering handlers and preserves original identity", async () => {
    const received: (string | undefined)[] = [];
    const chunks: unknown[] = [];
    const host = await connectUnit(
      defineProcessUnit({
        id: "receiver",
        imports: {},
        exports: { operations },
        setup: () => ({
          operations: operations.implement({
            read(input, context) {
              received.push(context.commandID);
              return input;
            },
            change(input, context) {
              received.push(context.commandID);
              return input;
            },
            async changeStream(input, context) {
              received.push(context.commandID);
              await context.emit(input);
              return input;
            },
          }),
        }),
      }),
      {},
      async (_peer, method, params) => {
        expect(method).toBe(wire.methods.chunk);
        exactFields(params, ["stream", "value"]);
        chunks.push(params.value);
        return true;
      },
    );
    const invoke = (member: string, commandID: unknown, stream?: string) =>
      host.request(
        wire.methods.call,
        {
          handle: "operations",
          member,
          input: 1,
          commandID,
          stream,
        },
        new AbortController().signal,
      );
    for (const commandID of [
      undefined,
      null,
      "",
      "a".repeat(PORTABLE_LIMITS.maxCommandIDCharacters + 1),
    ]) {
      await expect(invoke("change", commandID)).rejects.toMatchObject({
        code: wire.errors.contract,
        outcomeUnknown: false,
      });
    }
    for (const commandID of [null, "", "not-a-query"]) {
      await expect(invoke("read", commandID)).rejects.toMatchObject({
        code: wire.errors.contract,
        outcomeUnknown: false,
      });
    }
    expect(received).toEqual([]);
    expect(await invoke("read", undefined)).toBe(1);
    for (const commandID of identities) {
      expect(await invoke("change", commandID)).toBe(1);
      expect(await invoke("changeStream", commandID, "original-stream")).toBe(
        1,
      );
    }
    expect(received).toEqual([
      undefined,
      ...identities.flatMap((identity) => [identity, identity]),
    ]);
    expect(chunks).toEqual([1, 1]);
  });
});

describe("stream delivery ownership", () => {
  it("captures request fields once and keeps the original class consumer after dispatch", async () => {
    let client!: Client<typeof numbers>;
    const dispatched = Promise.withResolvers<void>();
    const deliver = Promise.withResolvers<void>();
    const controller = new AbortController();
    class Options implements RequestOptions {
      #chunks: unknown[] = [];
      signalReads = 0;
      consumerReads = 0;

      get signal() {
        this.signalReads++;
        return controller.signal;
      }

      get onChunk() {
        this.consumerReads++;
        return this.#consume;
      }

      #consume(value: unknown) {
        this.#chunks.push(value);
      }

      get chunks() {
        return this.#chunks;
      }
    }
    await connectUnit(
      defineProcessUnit({
        id: "captured-consumer",
        imports: { numbers },
        exports: {},
        setup({ imports }) {
          client = imports.numbers;
          return {};
        },
      }),
      serviceBinding("numbers", numbers),
      async (peer, _method, params, signal) => {
        exactFields(params, ["handle", "member", "input", "stream"]);
        dispatched.resolve();
        await deliver.promise;
        await peer.request(
          wire.methods.chunk,
          { stream: params.stream, value: params.input },
          signal,
        );
        return params.input;
      },
    );
    const options = new Options();
    const pending = client.count(7, options);
    await dispatched.promise;
    const replaced: unknown[] = [];
    Object.defineProperty(options, "onChunk", {
      value: (value: unknown) => replaced.push(value),
    });
    Object.defineProperty(options, "signal", {
      value: AbortSignal.abort(new Error("replacement signal")),
    });
    deliver.resolve();
    await expect(pending).resolves.toBe(7);
    expect(options.chunks).toEqual([7]);
    expect(replaced).toEqual([]);
    expect(options.signalReads).toBe(1);
    expect(options.consumerReads).toBe(1);
  });

  it("retains consumer rejection when the remote producer ignores the failed acknowledgement", async () => {
    let client!: Client<typeof numbers>;
    const failure = new Error("original consumer rejected the chunk");
    await connectUnit(
      defineProcessUnit({
        id: "consumer",
        imports: { numbers },
        exports: {},
        setup({ imports }) {
          client = imports.numbers;
          return {};
        },
      }),
      serviceBinding("numbers", numbers),
      async (peer, method, params, signal) => {
        expect(method).toBe(wire.methods.call);
        exactFields(params, ["handle", "member", "input", "stream"]);
        await peer
          .request(
            wire.methods.chunk,
            {
              stream: params.stream,
              value: params.input,
            },
            signal,
          )
          .catch(() => {});
        return params.input;
      },
    );
    await expect(
      client.count(7, {
        onChunk() {
          throw failure;
        },
      }),
    ).rejects.toMatchObject({
      outcomeUnknown: true,
      cause: failure,
    });
  });

  it.each(diagnosticFailures)(
    "retains the first consumer failure when $name inspection throws",
    async ({ fail }) => {
      let client!: Client<typeof numbers>;
      const failure = fail();
      await connectUnit(
        defineProcessUnit({
          id: "failed-diagnostic",
          imports: { numbers },
          exports: {},
          setup({ imports }) {
            client = imports.numbers;
            return {};
          },
        }),
        serviceBinding("numbers", numbers),
        async (peer, _method, params, signal) => {
          exactFields(params, ["handle", "member", "input", "stream"]);
          await peer
            .request(
              wire.methods.chunk,
              { stream: params.stream, value: params.input },
              signal,
            )
            .catch(() => {});
          return params.input;
        },
      );
      const result = await client
        .count(7, {
          onChunk() {
            throw failure;
          },
        })
        .catch((error: unknown) => error);
      expect(result).toBeInstanceOf(ProcessError);
      const error = result as ProcessError;
      expect(error.outcomeUnknown).toBe(true);
      expect(error.cause === failure).toBe(true);
      expect(Object.isFrozen(error)).toBe(true);
    },
  );

  it.each(diagnosticFailures)(
    "closes the original streams when a background job throws $name",
    async ({ fail }) => {
      const beginFailure = Promise.withResolvers<void>();
      const dispatched = Promise.withResolvers<void>();
      let client!: Client<typeof numbers>;
      const host = await connectUnit(
        defineProcessUnit({
          id: "failed-background-diagnostic",
          imports: { numbers },
          exports: {},
          setup({ imports, onActivate }) {
            client = imports.numbers;
            onActivate((work) => {
              work.start(async () => {
                await beginFailure.promise;
                throw fail();
              });
            });
            return {};
          },
        }),
        serviceBinding("numbers", numbers),
        async (_peer, _method, _params, signal) => {
          dispatched.resolve();
          if (!signal.aborted)
            await new Promise<void>((resolve) =>
              signal.addEventListener("abort", () => resolve(), { once: true }),
            );
          return 7;
        },
      );
      const closed = new Promise<void>((resolve) =>
        host.signal.addEventListener("abort", () => resolve(), { once: true }),
      );
      const pending = client.count(7, { onChunk() {} });
      const rejected = expect(pending).rejects.toMatchObject({
        outcomeUnknown: true,
      });
      await dispatched.promise;
      beginFailure.resolve();
      await rejected;
      await closed;
      expect(host.input.destroyed).toBe(true);
      expect(host.output.destroyed).toBe(true);
    },
  );

  it("joins an accepted consumer when a remote final result overtakes its acknowledgement", async () => {
    const entered = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    const final = Promise.withResolvers<void>();
    let client!: Client<typeof numbers>;
    let consumed = false;
    await connectUnit(
      defineProcessUnit({
        id: "consumer",
        imports: { numbers },
        exports: {},
        setup({ imports }) {
          client = imports.numbers;
          return {};
        },
      }),
      serviceBinding("numbers", numbers),
      async (peer, _method, params, signal) => {
        exactFields(params, ["handle", "member", "input", "stream"]);
        void peer
          .request(
            wire.methods.chunk,
            { stream: params.stream, value: params.input },
            signal,
          )
          .catch(() => {});
        await entered.promise;
        final.resolve();
        return params.input;
      },
    );
    let settled = false;
    const result = client
      .count(7, {
        async onChunk() {
          entered.resolve();
          await release.promise;
          consumed = true;
        },
      })
      .then(
        () => {
          settled = true;
          throw new Error("unacknowledged stream succeeded");
        },
        (error: unknown) => {
          settled = true;
          expect(consumed).toBe(true);
          return error;
        },
      );
    await final.promise;
    await new Promise<void>((resolve) => setImmediate(resolve));
    expect(settled).toBe(false);
    release.resolve();
    expect(await result).toMatchObject({ outcomeUnknown: true });
  });

  it("retains a failed emit when the local producer catches it and returns success", async () => {
    const host = await connectUnit(
      defineProcessUnit({
        id: "producer",
        imports: {},
        exports: { numbers },
        setup() {
          return {
            numbers: numbers.implement({
              async count(value, ctx) {
                await ctx.emit(value).catch(() => {});
                return value;
              },
            }),
          };
        },
      }),
      {},
      async () => {
        throw new ProcessError(
          wire.errors.contract,
          "consumer rejected the chunk",
        );
      },
    );
    await expect(
      host.request(
        wire.methods.call,
        {
          handle: "numbers",
          member: "count",
          input: 7,
          stream: "original-stream",
        },
        new AbortController().signal,
      ),
    ).rejects.toMatchObject({
      code: wire.errors.unknown,
      outcomeUnknown: true,
    });
  });

  it("joins an unawaited producer delivery and denies escaped emitters", async () => {
    const entered = Promise.withResolvers<void>();
    let emitLater!: () => Promise<void>;
    let chunks = 0;
    const host = await connectUnit(
      defineProcessUnit({
        id: "producer",
        imports: {},
        exports: { numbers },
        setup() {
          return {
            numbers: numbers.implement({
              async count(value, ctx) {
                emitLater = () => ctx.emit(value);
                if (value === 1) {
                  void ctx.emit(value);
                  await entered.promise;
                }
                return value;
              },
            }),
          };
        },
      }),
      {},
      async (_peer, _method, _params, signal) => {
        chunks++;
        entered.resolve();
        if (!signal.aborted)
          await new Promise<void>((resolve) =>
            signal.addEventListener("abort", () => resolve(), { once: true }),
          );
        return true;
      },
    );
    const request = (input: number) =>
      host.request(
        wire.methods.call,
        {
          handle: "numbers",
          member: "count",
          input,
          stream: "original-stream",
        },
        new AbortController().signal,
      );
    await expect(request(1)).rejects.toMatchObject({ outcomeUnknown: true });
    expect(await request(2)).toBe(2);
    await expect(emitLater()).rejects.toMatchObject({
      code: wire.errors.closed,
      outcomeUnknown: false,
    });
    expect(chunks).toBe(1);
  });
});

describe("post-dispatch failure evidence", () => {
  const commands = new Capability({
    protocol: PROTOCOL,
    reference: "commands",
    methods: [
      {
        name: "change",
        kind: "command",
        description: "",
        input: { type: "integer" },
        output: { type: "integer" },
      },
    ],
  });

  it.each([
    { code: wire.errors.cancelled, outcome: undefined },
    { code: wire.errors.contract, outcome: undefined },
    { code: wire.errors.contract, outcome: "unknown" as const },
  ])(
    "does not classify handler error $code with outcome $outcome as non-dispatch after an effect",
    async ({ code, outcome }) => {
      let effects = 0;
      const host = await connectUnit(
        defineProcessUnit({
          id: "mutation",
          imports: {},
          exports: { commands },
          setup() {
            return {
              commands: commands.implement({
                change() {
                  effects++;
                  throw new ProcessError(
                    code,
                    "handler failed after its effect",
                    { outcome },
                  );
                },
              }),
            };
          },
        }),
      );
      await expect(
        host.request(
          wire.methods.call,
          {
            handle: "commands",
            member: "change",
            commandID: "original-change",
            input: 1,
          },
          new AbortController().signal,
        ),
      ).rejects.toMatchObject({
        code: code === wire.errors.cancelled ? code : wire.errors.unknown,
        outcomeUnknown: true,
      });
      expect(effects).toBe(1);
      await expect(
        host.request(
          wire.methods.call,
          {
            handle: "commands",
            member: "change",
            commandID: "original-change",
            input: "invalid",
          },
          new AbortController().signal,
        ),
      ).rejects.toMatchObject({
        code: wire.errors.contract,
        outcomeUnknown: false,
      });
      expect(effects).toBe(1);
    },
  );

  it("classifies frame rejection before writing as non-dispatch even when cancellation immediately follows", async () => {
    const input = new PassThrough();
    const output = new PassThrough();
    const peer = new Peer(input, output);
    peer.limit(64);
    const controller = new AbortController();
    expect(() =>
      peer.request(
        wire.methods.call,
        { input: "a".repeat(128) },
        controller.signal,
      ),
    ).toThrow(
      expect.objectContaining({
        code: wire.errors.capacity,
        outcomeUnknown: false,
      }),
    );
    controller.abort(new Error("too late"));
    expect(output.readableLength).toBe(0);
    peer.stop(new Error("finished"));
  });

  it("preserves a confirmed result returned after cancellation instead of replacing it with an unknown reply", async () => {
    const input = new PassThrough();
    const output = new PassThrough();
    const peer = new Peer(input, output);
    const lines = createInterface({ input: output })[Symbol.asyncIterator]();
    const run = peer.run(async (_method, _params, signal) => {
      if (!signal.aborted)
        await new Promise<void>((resolve) =>
          signal.addEventListener("abort", () => resolve(), { once: true }),
        );
      return "confirmed";
    });
    input.write(
      JSON.stringify({ id: "1", method: wire.methods.call, params: {} }) + "\n",
    );
    input.write(
      JSON.stringify({ method: wire.methods.cancel, params: { id: "1" } }) +
        "\n",
    );
    expect(JSON.parse((await lines.next()).value!)).toEqual({
      id: "1",
      result: "confirmed",
    });
    input.end();
    await run;
  });
});
