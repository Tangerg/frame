import { captureProtocol } from "../protocol-capture.js";
import {
  createContext,
  createElement,
  useContext,
  type ComponentType,
  type ReactNode,
} from "react";
import { createRoot } from "react-dom/client";
import {
  FrameError,
  type Awaitable,
  type CloseOutcome,
  type Lifetime,
  type Contract,
} from "../index.js";
import type { Use } from "../browser/index.js";
import type { ObjectRef } from "../protocol.generated.js";

const Scope = createContext<Lifetime | undefined>(undefined);

export function useLifetime(): Lifetime {
  const lifetime = useContext(Scope);
  if (!lifetime)
    throw new FrameError(
      "invalid_plan",
      "component is outside its Frame lifetime",
    );
  return lifetime;
}

export interface ComponentBinding<P> {
  readonly owner: Lifetime;
  with<R>(work: (component: ComponentType<P>) => Awaitable<R>): Promise<R>;
}

export function bindComponent<P>(
  use: Use,
  reference: ObjectRef,
  contract: Contract<ComponentType<P>>,
): ComponentBinding<P> {
  if (contract.ref.mode !== "native")
    throw new FrameError(
      "contract_mismatch",
      "React components require a same-realm native contract",
    );
  const capturedReference = captureProtocol("ObjectRef", reference);
  const borrow = use.with.bind(use);
  return Object.freeze({
    owner: use.lifetime,
    with: <R>(work: (component: ComponentType<P>) => Awaitable<R>) =>
      borrow(capturedReference, contract, work),
  });
}

export interface MountOptions<P> {
  container: Element;
  props: P;
  onError?: (error: unknown) => void;
  wrap?: (children: ReactNode) => ReactNode;
}
export interface Mounted<P> {
  render(props: P): Promise<void>;
  close(signal?: AbortSignal): Promise<CloseOutcome>;
}

export async function mount<P extends object>(
  binding: ComponentBinding<P>,
  options: MountOptions<P>,
): Promise<Mounted<P>> {
  const borrow = binding.with.bind(binding);
  const { container, props, onError, wrap } = options;
  const owner = binding.owner.branch("mounted component");
  try {
    // Root release can unmount effects before their parent-owned work drains.
    const root = await owner.acquire(
      "react root",
      () =>
        createRoot(container, {
          onUncaughtError(error) {
            owner.beginClose();
            if (onError) onError.call(options, error);
            else globalThis.reportError?.(error);
          },
        }),
      (root) => root.unmount(),
    );
    const render = async (props: P) =>
      owner.run(() =>
        borrow((Component) => {
          owner.assertOpen();
          const node = createElement(Component, props);
          const content = wrap ? wrap.call(options, node) : node;
          return root.with((root) =>
            root.render(
              createElement(Scope.Provider, { value: owner }, content),
            ),
          );
        }),
      );
    await render(props);
    return Object.freeze({
      render,
      close: (signal?: AbortSignal) => owner.close(signal),
    });
  } catch (error) {
    await owner.close();
    throw error;
  }
}
