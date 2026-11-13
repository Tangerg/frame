// @vitest-environment happy-dom
import { act, useEffect, type ReactNode } from "react";
import { describe, expect, it } from "vitest";
import { Lifetime, Service, type Awaitable } from "../index.js";
import type { Use } from "../browser/index.js";
import type { ObjectRef } from "../protocol.generated.js";
import {
  bindComponent,
  mount,
  useLifetime,
  type ComponentBinding,
  type MountOptions,
} from "./index.ts";

(
  globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }
).IS_REACT_ACT_ENVIRONMENT = true;

describe("React adapter", () => {
  it("captures the original object reference before its caller can change the binding", async () => {
    const Preview = ({ title }: { title: string }) => <span>{title}</span>;
    const contract = new Service<typeof Preview>({
      name: "documents.captured-preview",
      version: "1",
      digest: "a".repeat(64),
      kind: "service",
      mode: "native",
    });
    const source = {
      objectId: "original-object",
      export: {
        instanceId: "original-instance",
        name: "preview",
        contract: { ...contract.ref },
      },
    };
    const expected = structuredClone(source);
    const reads = { object: 0, export: 0 };
    const reference: ObjectRef = {
      get objectId() {
        reads.object++;
        return source.objectId;
      },
      get export() {
        reads.export++;
        return source.export;
      },
    };
    const owner = new Lifetime("captured component use");
    const observed: ObjectRef[] = [];
    const use: Use = {
      id: "use",
      objects: [expected],
      lifetime: owner,
      with(received, receivedContract, work) {
        expect(this).toBe(use);
        observed.push(received);
        expect(receivedContract).toBe(contract);
        return owner.run((signal) => work(Preview as never, signal));
      },
      close: (signal) => owner.close(signal),
      wait: (signal) => owner.wait(signal),
    };
    const binding = bindComponent(use, reference, contract);
    use.with = () => {
      throw new Error("replacement borrow must not be called");
    };
    source.objectId = "replacement-object";
    source.export.instanceId = "replacement-instance";
    source.export.name = "replacement-export";
    source.export.contract.name = "replacement-contract";
    try {
      await expect(binding.with((component) => component)).resolves.toBe(
        Preview,
      );
      expect(observed).toEqual([expected]);
      expect(observed[0] === reference).toBe(false);
      expect(observed[0]!.export).not.toBe(source.export);
      expect(observed[0]!.export.contract).not.toBe(source.export.contract);
      expect(Object.isFrozen(observed[0])).toBe(true);
      expect(Object.isFrozen(observed[0]!.export)).toBe(true);
      expect(reads).toEqual({ object: 1, export: 1 });
    } finally {
      await owner.close();
    }
  });

  it("captures the binding and mount callbacks once with their receivers", async () => {
    const owner = new Lifetime("captured mount");
    const component = ({ title }: { title: string }) => <span>{title}</span>;
    let borrows = 0;
    let wraps = 0;
    const binding: ComponentBinding<{ title: string }> = {
      owner,
      get with() {
        borrows++;
        return function <R>(
          this: ComponentBinding<{ title: string }>,
          work: (value: typeof component) => Awaitable<R>,
        ) {
          expect(this).toBe(binding);
          return owner.run(() => work(component));
        };
      },
    };
    const options: MountOptions<{ title: string }> = {
      container: document.createElement("div"),
      props: { title: "original" },
      get wrap() {
        wraps++;
        if (wraps !== 1) throw new Error("mount must not reread its wrapper");
        return function (
          this: MountOptions<{ title: string }>,
          children: ReactNode,
        ) {
          expect(this).toBe(options);
          return <section>{children}</section>;
        };
      },
    };
    try {
      const view = await act(() => mount(binding, options));
      expect(options.container.innerHTML).toBe(
        "<section><span>original</span></section>",
      );
      await act(() => view.render({ title: "updated" }));
      expect(options.container.textContent).toBe("updated");
      expect({ borrows, wraps }).toEqual({ borrows: 1, wraps: 1 });
    } finally {
      await act(() => owner.close());
    }
  });

  it("captures the error observer before rendering can fail", async () => {
    const owner = new Lifetime("failed mount");
    const cause = new Error("component failed");
    function Broken(): never {
      throw cause;
    }
    let reads = 0;
    const reported: unknown[] = [];
    const observed = Promise.withResolvers<void>();
    const options: MountOptions<object> = {
      container: document.createElement("div"),
      props: {},
      get onError() {
        reads++;
        const read = reads;
        return function (this: MountOptions<object>, error: unknown) {
          expect(this).toBe(options);
          reported.push({ read, error });
          observed.resolve();
        };
      },
    };
    (
      globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = false;
    try {
      await mount({ owner, with: async (work) => work(Broken) }, options);
      await observed.promise;
      expect(reads).toBe(1);
      expect(reported).toEqual([{ read: 1, error: cause }]);
    } finally {
      await owner.close();
      (
        globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }
      ).IS_REACT_ACT_ENVIRONMENT = true;
    }
  });

  it("renders a native component while keeping async work owned after unmount", async () => {
    let finish!: () => void;
    let unmounted = false;
    function Preview({ title }: { title: string }) {
      const lifetime = useLifetime();
      useEffect(() => {
        void lifetime.run(
          () =>
            new Promise<void>((resolve) => {
              finish = resolve;
            }),
        );
        return () => {
          unmounted = true;
        };
      }, [lifetime]);
      return <div>{title}</div>;
    }
    const contract = new Service<typeof Preview>({
      name: "documents.react-preview",
      version: "1",
      digest: "a".repeat(64),
      kind: "service",
      mode: "native",
    });
    const reference: ObjectRef = {
      objectId: "object",
      export: {
        instanceId: "instance",
        name: "preview",
        contract: contract.ref,
      },
    };
    const lifetime = new Lifetime("component-use");
    const use: Use = {
      id: "use",
      objects: [reference],
      lifetime,
      with: (_reference, _contract, work) =>
        lifetime.run((signal) => work(Preview as never, signal)),
      close: (signal) => lifetime.close(signal),
      wait: (signal) => lifetime.wait(signal),
    };
    const container = document.createElement("div");
    document.body.append(container);
    let mounted!: Awaited<ReturnType<typeof mount<{ title: string }>>>;
    await act(async () => {
      mounted = await mount(bindComponent(use, reference, contract), {
        container,
        props: { title: "Document" },
        onError(error) {
          throw error;
        },
      });
    });
    expect(container.textContent).toBe("Document");
    let closed = false;
    let closing!: Promise<unknown>;
    await act(async () => {
      closing = mounted.close().then(() => {
        closed = true;
      });
      await Promise.resolve();
    });
    expect(unmounted).toBe(true);
    expect(closed).toBe(false);
    expect(container.textContent).toBe("");
    finish();
    await closing;
    expect(closed).toBe(true);
    expect(lifetime.closing).toBe(false);
    await expect(mounted.render({ title: "Retired" })).rejects.toThrow();
    await lifetime.close();
    container.remove();
  });
  it("retires sibling mounts independently and joins both on parent close", async () => {
    const owner = new Lifetime("shared preview");
    const component = ({ title }: { title: string }) => <span>{title}</span>;
    const binding = {
      owner,
      with: async <R,>(work: (value: typeof component) => R | PromiseLike<R>) =>
        owner.run(() => work(component)),
    };
    const left = document.createElement("div"),
      right = document.createElement("div");
    const views = await act(
      async () =>
        [
          await mount(binding, { container: left, props: { title: "left" } }),
          await mount(binding, { container: right, props: { title: "right" } }),
        ] as const,
    );
    expect([left.textContent, right.textContent]).toEqual(["left", "right"]);
    await act(async () => {
      await views[0].close();
      await views[1].render({ title: "updated" });
    });
    expect([left.textContent, right.textContent]).toEqual(["", "updated"]);
    await act(async () => {
      await owner.close();
    });
    expect(right.textContent).toBe("");
  });
});
