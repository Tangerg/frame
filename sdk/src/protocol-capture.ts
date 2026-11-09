import { freezeJSON } from "./json.js";
import {
  decodeProtocolValue,
  type ProtocolValues,
} from "./protocol.generated.js";

export function captureProtocol<K extends keyof ProtocolValues>(
  name: K,
  value: unknown,
): ProtocolValues[K] {
  return decodeProtocolValue(name, freezeJSON(value));
}
