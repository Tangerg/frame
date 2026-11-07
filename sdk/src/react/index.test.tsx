// @vitest-environment happy-dom
import { act, useEffect } from "react";
import { describe, expect, it } from "vitest";
import { Lifetime, Service } from "../index.js";
import type { Use } from "../browser/index.js";
import type { ObjectRef } from "../protocol.generated.js";
import { bindComponent, mount, useLifetime } from "./index.ts";

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
      with: (received, receivedContract, work) => {
        observed.push(received);
        expect(receivedContract).toBe(contract);
        return owner.run((signal) => work(Preview as never, signal));
      },
      close: (signal) => owner.close(signal),
      wait: (signal) => owner.wait(signal),
    };
    const binding = bindComponent(use, reference, contract);
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

  it("renders a native component while keeping async work owned after unmount", async () => {
    (
      globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = true;
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
