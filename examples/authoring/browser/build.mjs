import { mkdir, writeFile } from "node:fs/promises";
import { fileURLToPath, pathToFileURL } from "node:url";
import { join } from "node:path";
import { build } from "vite";

const directory = fileURLToPath(new URL(".", import.meta.url));
const output = join(directory, "dist");
await mkdir(output, { recursive: true });
for (const [entry, file] of [
  ["plugins.ts", "plugin.mjs"],
  ["client.ts", "client.mjs"],
]) {
  await build({
    root: directory,
    configFile: false,
    logLevel: "warn",
    build: {
      emptyOutDir: false,
      outDir: output,
      minify: false,
      lib: {
        entry: join(directory, "src", entry),
        formats: ["es"],
        fileName: () => file,
      },
    },
  });
}
const { declaration } = await import(
  pathToFileURL(join(output, "plugin.mjs")).href
);
await writeFile(
  join(output, "units.json"),
  JSON.stringify(declaration, null, 2) + "\n",
);
