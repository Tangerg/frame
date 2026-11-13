# Frame React

Bind an approved browser component once, then mount it independently into one or more containers.

```ts
import { bindComponent, mount } from "@frame/sdk/react";

const preview = bindComponent(use, previewRef, contracts.preview);
const left = await mount(preview, { container: leftPane, props });
const right = await mount(preview, { container: rightPane, props });
await left.close();
await right.render(updated);
```

Each mount owns a child lifetime of the approved Use. Unmounting a view releases only its work. Closing the parent cancels both views and retains the provider until child work drains. Components acquire the current mount lifetime with `useLifetime()`. Only same-Realm native components can be mounted.

Binding captures the approved Use's borrowing method and exact object reference once. Mounting captures the binding and options once, preserving callback receivers. A mount acquires its React root through the same Lifetime resource capability; root release unmounts effects while the mount still joins their asynchronous work.
