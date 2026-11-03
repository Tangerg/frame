import { Point, Service } from "@frame/sdk";
import {
  documentContracts,
  type Converted,
  type ConverterMetadata,
  type Diagnostic,
  type Document,
  type DocumentRef,
  type ValidatorMetadata,
} from "./contracts.generated";

// These are native JavaScript projections of the document consumer's contracts.
// Functions remain in this Realm. No Go function is serialized across a wire.
export type FileReader = (
  reference: DocumentRef,
  signal: AbortSignal,
) => Promise<Document>;
export type Converter = (
  reference: DocumentRef,
  signal: AbortSignal,
) => Promise<Converted>;
export type Validator = (
  document: Converted,
  signal: AbortSignal,
) => Promise<readonly Diagnostic[]>;

export const files = new Service<FileReader>(documentContracts.files);
export const converters = new Point<ConverterMetadata, Converter>(
  documentContracts.converters,
);
export const validators = new Point<ValidatorMetadata, Validator>(
  documentContracts.validators,
);
export const markdownFormat = "markdown";

export async function sha256(bytes: Uint8Array<ArrayBuffer>): Promise<string> {
  return Array.from(
    new Uint8Array(await crypto.subtle.digest("SHA-256", bytes)),
    (value) => value.toString(16).padStart(2, "0"),
  ).join("");
}
