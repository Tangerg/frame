import { PassThrough, Writable } from "node:stream";
import { describe, expect, it, onTestFinished } from "vitest";
import { Peer } from "./peer.js";
import wire from "./wire.json" with { type: "json" };

function connectedPeer() {
  const input = new PassThrough();
  const frames: Array<{ id: string; method: string; params: unknown }> = [];
  const output = new Writable({
    write(chunk, _encoding, done) {
      frames.push(JSON.parse(chunk.toString()));
      done();
    },
  });
  const peer = new Peer(input, output);
  const running = peer.run(async () => true).catch((error: unknown) => error);
  onTestFinished(async () => {
    peer.stop(new Error("test finished"));
    expect(await running).toMatchObject({ code: "ERR_STREAM_PREMATURE_CLOSE" });
  });
  const reply = (id: string, result: unknown) =>
    input.write(JSON.stringify({ id, result }) + "\n");
  return { peer, frames, reply };
}

describe("process request admission", () => {
  it("closes the session after a write failure instead of reusing uncertain slots", async () => {
    const input = new PassThrough();
    const failure = new Error("write interrupted");
    const output = new Writable({
      write(_chunk, _encoding, done) {
        done(failure);
      },
    });
    const peer = new Peer(input, output);
    const running = peer.run(async () => true).catch((error: unknown) => error);
    await expect(
      peer.request(wire.methods.call, {}, new AbortController().signal),
    ).rejects.toMatchObject({ outcomeUnknown: true });
    expect(peer.signal.aborted).toBe(true);
    expect(input.destroyed).toBe(true);
    expect(output.destroyed).toBe(true);
    expect(() =>
      peer.request(wire.methods.call, {}, new AbortController().signal),
    ).toThrow(
      expect.objectContaining({
        code: wire.errors.closed,
        outcomeUnknown: false,
      }),
    );
    expect(await running).toMatchObject({ code: "ERR_STREAM_PREMATURE_CLOSE" });
  });

  it("rechecks cancellation after encoding without consuming a request identity", async () => {
    const { peer, frames, reply } = connectedPeer();
    const controller = new AbortController();
    const reason = new Error("cancelled by input getter");
    expect(() =>
      peer.request(
        wire.methods.call,
        {
          get input() {
            controller.abort(reason);
            return "value";
          },
        },
        controller.signal,
      ),
    ).toThrow(
      expect.objectContaining({
        code: wire.errors.cancelled,
        cause: reason,
        outcomeUnknown: false,
      }),
    );
    expect(frames).toEqual([]);
    const accepted = peer.request(
      wire.methods.call,
      {},
      new AbortController().signal,
    );
    expect(frames[0]?.id).toBe("1");
    reply("1", true);
    await expect(accepted).resolves.toBe(true);
  });

  it("refuses a peer closed by a getter before attempting the write", () => {
    const { peer, frames } = connectedPeer();
    const reason = new Error("closed by input getter");
    expect(() =>
      peer.request(
        wire.methods.call,
        {
          get input() {
            peer.stop(reason);
            return "value";
          },
        },
        new AbortController().signal,
      ),
    ).toThrow(
      expect.objectContaining({
        code: wire.errors.closed,
        cause: reason,
        outcomeUnknown: false,
      }),
    );
    expect(frames).toEqual([]);
  });

  it("assigns request identities after reentrant getters have finished", async () => {
    const { peer, frames, reply } = connectedPeer();
    let nested!: Promise<unknown>;
    let reads = 0;
    const outer = peer.request(
      wire.methods.call,
      {
        get input() {
          reads++;
          nested = peer.request(
            wire.methods.call,
            { input: "nested" },
            new AbortController().signal,
          );
          return "outer";
        },
      },
      new AbortController().signal,
    );
    expect(reads).toBe(1);
    expect(frames).toEqual([
      { id: "1", method: wire.methods.call, params: { input: "nested" } },
      { id: "2", method: wire.methods.call, params: { input: "outer" } },
    ]);
    reply("1", "nested result");
    reply("2", "outer result");
    await expect(nested).resolves.toBe("nested result");
    await expect(outer).resolves.toBe("outer result");
  });

  it("rechecks capacity consumed by a reentrant getter", async () => {
    const { peer, frames, reply } = connectedPeer();
    const accepted: Promise<unknown>[] = [];
    expect(() =>
      peer.request(
        wire.methods.call,
        {
          get input() {
            for (let index = 0; index < wire.maxInFlight; index++)
              accepted.push(
                peer.request(
                  wire.methods.call,
                  { input: index },
                  new AbortController().signal,
                ),
              );
            return "excess";
          },
        },
        new AbortController().signal,
      ),
    ).toThrow(
      expect.objectContaining({
        code: wire.errors.capacity,
        outcomeUnknown: false,
      }),
    );
    expect(frames).toHaveLength(wire.maxInFlight);
    for (const frame of frames) reply(frame.id, true);
    await expect(Promise.all(accepted)).resolves.toHaveLength(wire.maxInFlight);
  });

  it("retains cancelled dispatched slots until their original replies arrive", async () => {
    const { peer, frames, reply } = connectedPeer();
    const owner = new AbortController();
    const first = peer.request(wire.methods.call, {}, owner.signal);
    const cancelled = expect(first).rejects.toMatchObject({
      code: wire.errors.cancelled,
      outcomeUnknown: true,
    });
    owner.abort(new Error("caller stopped waiting"));
    await cancelled;
    const accepted: Promise<unknown>[] = [];
    for (let index = 1; index < wire.maxInFlight; index++)
      accepted.push(
        peer.request(wire.methods.call, {}, new AbortController().signal),
      );
    expect(() =>
      peer.request(wire.methods.call, {}, new AbortController().signal),
    ).toThrow(
      expect.objectContaining({
        code: wire.errors.capacity,
        outcomeUnknown: false,
      }),
    );
    for (const frame of frames) if (frame.id) reply(frame.id, true);
    await Promise.all(accepted);
    const next = peer.request(
      wire.methods.call,
      {},
      new AbortController().signal,
    );
    reply(String(wire.maxInFlight + 1), true);
    await expect(next).resolves.toBe(true);
  });
});
