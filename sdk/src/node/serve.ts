/// <reference types="node" preserve="true" />
import type { Readable, Writable } from "node:stream";
import { Instance } from "./instance.js";
import { Peer } from "./peer.js";
import {
  defineProcessUnit,
  type Exports,
  type Imports,
  type ProcessUnit,
} from "./unit.js";

export async function serve<I extends Imports, E extends Exports>(
  unit: ProcessUnit<I, E>,
  options: { input?: Readable; output?: Writable } = {},
): Promise<void> {
  const peer = new Peer(
    options.input ?? process.stdin,
    options.output ?? process.stdout,
  );
  const instance = new Instance(defineProcessUnit(unit), peer);
  peer.signal.addEventListener("abort", () => instance.lifetime.beginClose(), {
    once: true,
  });
  try {
    await peer.run((name, params, signal) =>
      instance.dispatch(name, params, signal),
    );
  } finally {
    await instance.lifetime.close();
  }
}
