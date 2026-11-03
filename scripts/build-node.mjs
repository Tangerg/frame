import { copyFile } from "node:fs/promises";

const here = import.meta.url;
await Promise.all(
  [
    "process/wire.json",
    "process/shapes.generated.json",
    "portable/profile.generated.json",
  ].map((path) =>
    copyFile(
      new URL("../" + path, here),
      new URL("../sdk/src/node/" + path.split("/").at(-1), here),
    ),
  ),
);
