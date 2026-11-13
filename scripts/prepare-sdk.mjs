import { copyFile, rm } from "node:fs/promises";

const here = import.meta.url;
await rm(new URL("../sdk/dist/", here), { recursive: true, force: true });
await Promise.all(
  [
    ["process/wire.json", "wire.json"],
    ["process/shapes.generated.json", "shapes.generated.json"],
    ["contract-profile.generated.json", "profile.generated.json"],
  ].map(([source, target]) =>
    copyFile(
      new URL("../" + source, here),
      new URL("../sdk/src/node/" + target, here),
    ),
  ),
);
