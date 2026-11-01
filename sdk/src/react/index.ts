import {
  createContext,
  createElement,
  useContext,
  type ComponentType,
  type ReactNode,
} from "react";
import { createRoot, type Root } from "react-dom/client";
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
  return Object.freeze({
    owner: use.lifetime,
    with: <R>(work: (component: ComponentType<P>) => Awaitable<R>) =>
      use.with(reference, contract, work),
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
  const owner = binding.owner.branch("mounted component");
  // A child of the approved Use keeps its objects protected while mounted work
  // drains. Unmount runs in a separate branch, so it can interrupt effect work.
  const rendering = owner.branch("react root");
  let root: Root | undefined;
  const render = async (props: P) =>
    owner.run(() =>
      binding.with((Component) => {
        owner.assertOpen();
        const node = createElement(Component, props);
        root!.render(
          createElement(
            Scope.Provider,
            { value: owner },
            options.wrap ? options.wrap(node) : node,
          ),
        );
      }),
    );
  try {
    rendering.cleanup(() => {
      root?.unmount();
      root = undefined;
    }, "react root");
    root = createRoot(options.container, {
      onUncaughtError(error) {
        owner.beginClose();
        if (options.onError) options.onError(error);
        else globalThis.reportError?.(error);
      },
    });
    await render(options.props);
  } catch (error) {
    await owner.close();
    throw error;
  }
  return Object.freeze({
    render,
    close: (signal?: AbortSignal) => owner.close(signal),
  });
}
