import { PassThrough } from "node:stream";
import { expect, it, onTestFinished } from "vitest";
import { Capability, defineProcessUnit, serve } from "./index.js";
import { Peer } from "./peer.js";
import { PROTOCOL, type Handlers } from "./contract.js";
import type { Lifetime } from "../lifetime.js";
import wire from "./wire.json" with { type: "json" };

const readings = new Capability({
  protocol: PROTOCOL,
  reference: "activation.readings",
  methods: [
    {
      name: "read",
      kind: "query",
      description: "",
      input: { type: "integer" },
      output: { type: "integer" },
    },
  ],
});

async function preparedUnit(
  activate: (lifetime: Lifetime) => Promise<void>,
  read: Handlers<typeof readings>["read"],
): Promise<Peer> {
  const input = new PassThrough();
  const output = new PassThrough();
  const serving = serve(
    defineProcessUnit({
      id: "activation-provider",
      imports: {},
      exports: { readings },
      setup({ lifetime, onActivate }) {
        onActivate(() => activate(lifetime));
        return { readings: { read } };
      },
    }),
    { input, output },
  );
  const host = new Peer(output, input);
  const running = host.run(async () => true);
  onTestFinished(async () => {
    host.stop(new Error("activation test completed"));
    await Promise.allSettled([serving, running]);
  });
  const signal = new AbortController().signal;
  await host.request(
    wire.methods.hello,
    { version: wire.version, maxFrameBytes: wire.maxFrameBytes },
    signal,
  );
  await host.request(wire.methods.prepare, { imports: {} }, signal);
  return host;
}

it("admits exported calls only after the original activation completes", async () => {
  const entered = Promise.withResolvers<void>();
  const finish = Promise.withResolvers<void>();
  onTestFinished(() => finish.resolve());
  let calls = 0;
  const host = await preparedUnit(
    async () => {
      entered.resolve();
      await finish.promise;
    },
    (value) => {
      calls++;
      return value;
    },
  );
  const signal = new AbortController().signal;
  const activation = host.request(wire.methods.activate, {}, signal);
  await entered.promise;
  const call = () =>
    host.request(
      wire.methods.call,
      { handle: "readings", member: "read", input: 7 },
      signal,
    );
  try {
    await expect(call()).rejects.toMatchObject({
      code: wire.errors.closed,
      outcomeUnknown: false,
    });
    expect(calls).toBe(0);
  } finally {
    finish.resolve();
    await activation;
  }
  await expect(call()).resolves.toBe(7);
  expect(calls).toBe(1);
});

it.each(["protocol close", "original lifetime"])(
  "cannot acknowledge activation after %s closes admission",
  async (close) => {
    const entered = Promise.withResolvers<Lifetime>();
    const finish = Promise.withResolvers<void>();
    onTestFinished(() => finish.resolve());
    const host = await preparedUnit(
      async (lifetime) => {
        entered.resolve(lifetime);
        await finish.promise;
      },
      (value) => value,
    );
    const signal = new AbortController().signal;
    const activation = host.request(wire.methods.activate, {}, signal);
    const rejected = expect(activation).rejects.toMatchObject({
      code: wire.errors.closed,
    });
    const lifetime = await entered.promise;
    let closing: Promise<unknown> | undefined;
    if (close === "protocol close") {
      const sealed = new Promise<void>((resolve) => {
        lifetime.signal.addEventListener("abort", () => resolve(), {
          once: true,
        });
      });
      closing = host.request(wire.methods.close, {}, signal);
      await sealed;
    } else {
      lifetime.beginClose();
    }
    finish.resolve();
    await rejected;
    if (closing) await expect(closing).resolves.toBe(true);
  },
);
