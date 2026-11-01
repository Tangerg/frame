import { copyFile } from "node:fs/promises";
for (const name of ["wire.json", "portable-profile.generated.json"]) {
  await copyFile(
    new URL(`../protocol/process/${name}`, import.meta.url),
    new URL(`../packages/node/src/${name}`, import.meta.url),
  );
}
