import { describe, expect, it, vi } from "vitest";
import { Lifetime } from "../index.js";
import {
  PROTOCOL,
  type LocalPlan,
  type ReadyReport,
} from "../protocol.generated.js";
import { HTTPControl, type HTTPControlOptions } from "./http";
import { readResponseBytes } from "./response";

const ready: ReadyReport = {
  operationId: "ready",
  authority: { id: "host", epoch: "1" },
  realm: { id: "realm", epoch: "1" },
  execution: { id: "execution", generation: "1" },
  objects: [],
  contributions: [],
};
const emptyPoll = {
  authority: ready.authority,
  realm: ready.realm,
  plans: [],
  fences: [],
};
function control(fetch: typeof globalThis.fetch, maximumMessageBytes = 4096) {
  const lifetime = new Lifetime("control");
  return {
    lifetime,
    control: new HTTPControl({
      endpoint: "http://localhost/control",
      credential: "realm-secret",
      maximumMessageBytes,
      lifetime,
      fetch,
    }),
  };
}

describe("authenticated control transport", () => {
  it("uses one validated options capture for requests, callback receiver and cleanup", async () => {
    const original = new Lifetime("original transport owner");
    const replacement = new Lifetime("later getter owner");
    class Options implements HTTPControlOptions {
      readonly reads = {
        endpoint: 0,
        credential: 0,
        capacity: 0,
        lifetime: 0,
        fetch: 0,
      };
      readonly #requests: Parameters<typeof globalThis.fetch>[] = [];

      get endpoint() {
        return ++this.reads.endpoint === 1
          ? "http://localhost/control"
          : "http://invalid.example/control";
      }
      get credential() {
        return ++this.reads.credential === 1
          ? "captured-secret"
          : "later-secret";
      }
      get maximumMessageBytes() {
        return ++this.reads.capacity === 1 ? 4096 : 1;
      }
      get lifetime() {
        return ++this.reads.lifetime === 1 ? original : replacement;
      }
      get fetch(): typeof globalThis.fetch {
        this.reads.fetch++;
        return this.#request;
      }
      get requests() {
        return this.#requests;
      }
      #request(...args: Parameters<typeof globalThis.fetch>) {
        this.#requests.push(args);
        return Promise.resolve(Response.json(emptyPoll));
      }
    }
    const options = new Options();
    const transport = new HTTPControl(options);
    const laterFetch = vi.fn<typeof globalThis.fetch>();
    Object.defineProperty(options, "fetch", { value: laterFetch });
    try {
      await expect(transport.poll()).resolves.toEqual(emptyPoll);
      expect(options.requests).toHaveLength(1);
      expect(options.requests[0]).toEqual([
        "http://localhost/control",
        expect.objectContaining({
          headers: expect.objectContaining({
            Authorization: "Bearer captured-secret",
          }),
          signal: expect.any(AbortSignal),
        }),
      ]);
      expect(options.reads).toEqual({
        endpoint: 1,
        credential: 1,
        capacity: 1,
        lifetime: 1,
        fetch: 1,
      });
      expect(laterFetch).not.toHaveBeenCalled();
      await original.close();
      await expect(transport.poll()).rejects.toMatchObject({ code: "closed" });
      expect(replacement.signal.aborted).toBe(false);
    } finally {
      await original.close();
      await replacement.close();
    }
  });

  it.each([false, true])(
    "invokes native fetch with its global receiver (explicit: %s)",
    async (explicit) => {
      const lifetime = new Lifetime("native browser transport");
      const fetch = vi.fn(function (this: typeof globalThis) {
        if (this !== globalThis) throw new TypeError("Illegal invocation");
        return Promise.resolve(Response.json(emptyPoll));
      });
      vi.stubGlobal("fetch", fetch);
      try {
        const transport = new HTTPControl({
          endpoint: "http://localhost/control",
          credential: "realm-secret",
          maximumMessageBytes: 4096,
          lifetime,
          ...(explicit ? { fetch: globalThis.fetch } : {}),
        });
        await transport.poll();
        expect(fetch).toHaveBeenCalledOnce();
      } finally {
        await lifetime.close();
        vi.unstubAllGlobals();
      }
    },
  );

  it("sends the explicit realm credential and envelope without cookies or redirects", async () => {
    const fetch = vi.fn<typeof globalThis.fetch>(async () =>
      Response.json(emptyPoll),
    );
    const f = control(fetch);
    await f.control.poll();
    const [url, request] = fetch.mock.calls[0]!;
    expect(url).toBe("http://localhost/control");
    expect(request).toMatchObject({
      method: "POST",
      redirect: "error",
      credentials: "omit",
      headers: { Authorization: "Bearer realm-secret" },
    });
    expect(JSON.parse(request!.body as string)).toEqual({
      method: "poll",
      input: {},
    });
    expect(request!.signal).toBeInstanceOf(AbortSignal);
    expect(request!.signal!.aborted).toBe(false);
    await f.lifetime.close();
    expect(request!.signal!.aborted).toBe(true);
    await expect(f.control.poll()).rejects.toMatchObject({ code: "closed" });
  });

  it("closes the original selection lease through the authenticated control port", async () => {
    const fetch = vi.fn<typeof globalThis.fetch>(
      async () => new Response("null"),
    );
    const f = control(fetch);
    await f.control.closeSnapshot("original-snapshot");
    const request = fetch.mock.calls[0]![1]!;
    expect(JSON.parse(request.body as string)).toEqual({
      method: "closeSnapshot",
      input: "original-snapshot",
    });
    expect(request.headers).toMatchObject({
      Authorization: "Bearer realm-secret",
    });
    await f.lifetime.close();
  });

  it("sends the caller's snapshot identity and preserves an unknown capture outcome", async () => {
    const fetch = vi.fn<typeof globalThis.fetch>(async () => new Response("{"));
    const f = control(fetch);
    const capture = {
      operationId: "original-snapshot",
      points: ["document.previews"],
    };
    for (let attempt = 0; attempt < 2; attempt++)
      await expect(f.control.snapshot(capture)).rejects.toMatchObject({
        code: "outcome_unknown",
      });
    for (const [, request] of fetch.mock.calls)
      expect(JSON.parse(request!.body as string)).toEqual({
        method: "snapshot",
        input: capture,
      });
    await f.lifetime.close();
  });

  it("treats a lost preparation admission response as an unknown mutation", async () => {
    const fetch = vi.fn<typeof globalThis.fetch>(async () => {
      throw new Error("response lost");
    });
    const f = control(fetch);
    const plan: LocalPlan = {
      protocol: PROTOCOL,
      approvalToken: "original-approval",
      authority: ready.authority,
      realm: ready.realm,
      execution: ready.execution,
      scopeId: "original-scope",
      scopeGeneration: "1",
      units: [],
    };
    await expect(f.control.admitPlan(plan)).rejects.toMatchObject({
      code: "outcome_unknown",
    });
    expect(JSON.parse(fetch.mock.calls[0]![1]!.body as string)).toEqual({
      method: "admitPlan",
      input: plan,
    });
    await f.lifetime.close();
  });

  it("bounds both messages and preserves unknown mutation settlement on malformed responses", async () => {
    const fetch = vi.fn<typeof globalThis.fetch>(
      async () => new Response("x".repeat(128)),
    );
    const f = control(fetch, 64);
    await expect(f.control.ready(ready)).rejects.toMatchObject({
      code: "capacity",
    });
    expect(fetch).not.toHaveBeenCalled();
    await expect(f.control.poll()).rejects.toMatchObject({
      code: "unavailable",
    });
    const changed = control(vi.fn(async () => new Response("{")));
    await expect(changed.control.ready(ready)).rejects.toMatchObject({
      code: "outcome_unknown",
    });
    await f.lifetime.close();
    await changed.lifetime.close();
  });

  it("joins in-flight fetches when the bootstrap transport owner closes", async () => {
    let started!: () => void;
    const entered = new Promise<void>((resolve) => {
      started = resolve;
    });
    const fetch = vi.fn<typeof globalThis.fetch>(
      (_url, options) =>
        new Promise<Response>((_resolve, reject) => {
          options!.signal!.addEventListener(
            "abort",
            () => reject(options!.signal!.reason),
            { once: true },
          );
          started();
        }),
    );
    const f = control(fetch);
    const request = expect(f.control.poll()).rejects.toMatchObject({
      code: "unavailable",
    });
    await entered;
    expect((await f.lifetime.close()).status).toBe("released");
    await request;
    expect(fetch).toHaveBeenCalledOnce();
  });
});

it("bounds streamed bytes across chunks and cancels an oversized body", async () => {
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(Uint8Array.of(1));
      controller.enqueue(new Uint8Array());
      controller.enqueue(Uint8Array.of(2, 3));
      controller.close();
    },
  });
  expect(await readResponseBytes(new Response(body), 3)).toEqual(
    Uint8Array.of(1, 2, 3),
  );
  const cancel = vi.fn();
  const overflow = new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(Uint8Array.of(1, 2));
    },
    cancel,
  });
  await expect(readResponseBytes(new Response(overflow), 1)).rejects.toThrow(
    "byte limit",
  );
  expect(cancel).toHaveBeenCalledOnce();
  expect(overflow.locked).toBe(false);
});
