import { describe, expect, it, vi } from "vitest";
import { Lifetime } from "@frame/sdk";
import { type ReadyReport } from "@frame/sdk/protocol";
import { HTTPControl } from "./http";

const ready: ReadyReport = {
  operationId: "ready",
  authority: { id: "host", epoch: "1" },
  realm: { id: "realm", epoch: "1" },
  execution: { id: "execution", generation: "1" },
  objects: [],
  contributions: [],
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
  it("invokes the native fetch with its owning global receiver", async () => {
    const lifetime = new Lifetime("native browser transport");
    const fetch = vi.fn(function (this: typeof globalThis) {
      if (this !== globalThis) throw new TypeError("Illegal invocation");
      return Promise.resolve(new Response("null"));
    });
    vi.stubGlobal("fetch", fetch);
    try {
      const transport = new HTTPControl({
        endpoint: "http://localhost/control",
        credential: "realm-secret",
        maximumMessageBytes: 4096,
        lifetime,
      });
      await transport.poll();
      expect(fetch).toHaveBeenCalledOnce();
    } finally {
      await lifetime.close();
      vi.unstubAllGlobals();
    }
  });

  it("sends the explicit realm credential and envelope without cookies or redirects", async () => {
    const fetch = vi.fn<typeof globalThis.fetch>(
      async () => new Response("null"),
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
    expect(request!.signal).toBe(f.lifetime.signal);
    await f.lifetime.close();
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
