import { Decimal } from "decimal.js";
import {
  Validator,
  ValidatorResult,
  type CustomProperty,
  type Schema,
} from "jsonschema";
import { compilePattern } from "./pattern.js";
import { checkFormat, validateFormat } from "./format.js";
import profile from "./profile.generated.json" with { type: "json" };
import {
  parseJSON,
  stringifyJSON,
  isJSONNumber as numeric,
  equalJSON as equal,
  isJSONInteger,
  compareJSONNumbers,
  type JSONNumber,
} from "../json.js";
import {
  checkDescriptor,
  checkValue,
  exceeded,
  PORTABLE_LIMITS,
} from "./limits.js";

type RecordValue = Record<string, unknown>;

export class SchemaMismatch extends Error {
  constructor() {
    super("plugin: value does not satisfy its schema");
    this.name = "SchemaMismatch";
  }
}

const maximumNumberMagnitude =
  PORTABLE_LIMITS.maxAbsExponent + PORTABLE_LIMITS.maxNumberBytes;
// The admitted profile bounds every coefficient and exponent before arithmetic.
// A private constructor keeps unrelated Decimal configuration from changing a verdict.
const ProfileDecimal = Decimal.clone({
  precision: 2 * maximumNumberMagnitude + 1,
  minE: -maximumNumberMagnitude,
  maxE: maximumNumberMagnitude,
  rounding: Decimal.ROUND_DOWN,
  modulo: Decimal.ROUND_DOWN,
});

function multipleOf(value: JSONNumber, divisor: JSONNumber): boolean {
  return new ProfileDecimal(value.toString()).mod(divisor.toString()).isZero();
}

interface SchemaKeyword {
  children?: "map" | "schema" | "array";
  edge?: "same" | "descend";
  value?: string;
  annotation?: boolean;
}
const keywords: Readonly<Record<string, SchemaKeyword>> = profile.schema
  .keywords as Record<string, SchemaKeyword>;

function pointerToken(key: string): string {
  return key.replaceAll("~", "~0").replaceAll("/", "~1");
}
function schemaObject(value: unknown): value is RecordValue {
  return (
    value !== null &&
    typeof value === "object" &&
    !Array.isArray(value) &&
    !numeric(value)
  );
}
function localReference(value: unknown): string {
  if (typeof value !== "string" || (value !== "#" && !value.startsWith("#/")))
    throw new Error("portable: schema references must be local JSON pointers");
  return value;
}
interface Subschema {
  value: unknown;
  suffix: string;
  edge?: "same" | "descend";
}
function* subschemas(key: string, field: unknown): Generator<Subschema> {
  const keyword = keywords[key];
  if (keyword?.children === "map") {
    if (!schemaObject(field))
      throw new Error("portable: schema map must be an object");
    for (const [name, value] of Object.entries(field))
      yield { value, suffix: "/" + pointerToken(name), edge: keyword.edge };
  } else if (keyword?.children === "schema") {
    yield { value: field, suffix: "", edge: keyword.edge };
  } else if (keyword?.children === "array") {
    if (!Array.isArray(field) || field.length === 0)
      throw new Error("portable: schema composition requires a nonempty array");
    for (const [index, value] of field.entries())
      yield { value, suffix: "/" + index, edge: keyword.edge };
  }
}

function admitNumericConstraints(
  schema: RecordValue,
  key: string,
  field: unknown,
): void {
  const shape = keywords[key]!.value;
  if (
    (shape === "number" || shape === "positive-number") &&
    (!numeric(field) ||
      (shape === "positive-number" && compareJSONNumbers(field, 0) <= 0))
  )
    throw new Error("plugin: numeric schema constraint is invalid");
  if (shape === "size") {
    if (
      !numeric(field) ||
      !isJSONInteger(field) ||
      compareJSONNumbers(field, 0) < 0 ||
      compareJSONNumbers(field, PORTABLE_LIMITS.maxSizeConstraint) > 0
    )
      throw new Error("plugin: schema size constraint is invalid");
    // Only the engine projection uses JS numbers. The published identity retains its lexeme.
    schema[key] = Number(field.toString());
  }
}

function admitChoiceConstraints(key: string, field: unknown): void {
  if (
    keywords[key]!.value === "required" &&
    (!Array.isArray(field) ||
      field.some((name) => typeof name !== "string") ||
      new Set(field).size !== field.length)
  )
    throw new Error("plugin: schema required members are invalid");
  if (
    keywords[key]!.value === "enum" &&
    (!Array.isArray(field) || field.length === 0)
  )
    throw new Error("plugin: schema enum is invalid");
  if (
    keywords[key]!.value === "enum" &&
    (field as unknown[]).some((item, index, values) =>
      values.slice(0, index).some((previous) => equal(item, previous)),
    )
  )
    throw new Error("plugin: schema enum has duplicate values");
  if (keywords[key]!.value === "types") {
    const types = Array.isArray(field) ? field : [field];
    if (
      types.length === 0 ||
      types.some(
        (type) =>
          typeof type !== "string" || !profile.schema.types.includes(type),
      ) ||
      new Set(types).size !== types.length
    )
      throw new Error("plugin: schema type is invalid");
  }
}

function admitAnnotations(
  key: string,
  field: unknown,
  patterns: Map<string, RegExp>,
): void {
  const shape = keywords[key]!.value;
  if (shape === "string" && typeof field !== "string")
    throw new Error("plugin: schema string constraint is invalid");
  if (shape === "boolean" && typeof field !== "boolean")
    throw new Error("plugin: schema boolean constraint is invalid");
  if (shape === "array" && !Array.isArray(field))
    throw new Error("plugin: schema annotation must be an array");
  if (key === "$schema" && field !== profile.schema.dialect)
    throw new Error("plugin: unsupported schema dialect");
  if (key === "format") checkFormat(field);
  if (key === "pattern") {
    patterns.set(field as string, compilePattern(field as string));
  }
}

function admitSchema(value: unknown, patterns: Map<string, RegExp>): void {
  if (typeof value === "boolean") return;
  if (!schemaObject(value))
    throw new Error("plugin: schema must be an object or boolean");
  for (const [key, field] of Object.entries(value)) {
    if (!Object.hasOwn(keywords, key))
      throw new Error(`plugin: unsupported schema keyword ${key}`);
    if (key === "patternProperties") {
      for (const name of Object.keys(field as RecordValue))
        patterns.set(name, compilePattern(name));
    }
    for (const child of subschemas(key, field))
      admitSchema(child.value, patterns);
    admitNumericConstraints(value, key, field);
    admitChoiceConstraints(key, field);
    admitAnnotations(key, field, patterns);
  }
  if (value.$ref !== undefined) {
    const ref = localReference(value.$ref);
    delete value.$ref;
    value.allOf = [
      ...((value.allOf as unknown[] | undefined) ?? []),
      Object.assign(Object.create(null), { $ref: ref }),
    ];
  }
}

// The engine indexes schema maps with ordinary property lookup and may rewrite
// instance members. Private prototype-free JSON trees preserve own-member
// semantics without reserving otherwise legal names or mutating caller data.
function engineProjection(source: unknown): unknown {
  const document = parseJSON(stringifyJSON(source));
  function visit(value: unknown): void {
    if (Array.isArray(value)) value.forEach(visit);
    else if (schemaObject(value)) {
      Object.setPrototypeOf(value, null);
      Object.values(value).forEach(visit);
    }
  }
  visit(document);
  return document;
}

// jsonschema supplies traversal and composition. Its supported instance-local
// hooks adapt numeric comparison to the public exact JSON representation.
export function compileSchema(source: unknown): (value: unknown) => void {
  checkDescriptor(source);
  const document = engineProjection(source);
  const budget = schemaWork(document);
  const validator = new Validator() as Validator & {
    types: Record<string, (value: unknown) => boolean>;
  };
  validator.types.number = numeric;
  validator.types.integer = isJSONInteger;
  validator.types.object = (value) =>
    value !== null &&
    typeof value === "object" &&
    !Array.isArray(value) &&
    !numeric(value);
  // The engine's legacy not handler coerces subschemas to property keys.
  // Same-instance validation preserves prototype-free schemas and local refs.
  validator.attributes.not = ((value, schema, options, ctx) => {
    if (value === undefined) return "";
    return validator.validate(value, schema.not!, options, ctx).valid
      ? "not constraint failed"
      : "";
  }) as CustomProperty;
  const patterns = new Map<string, RegExp>();
  validator.attributes.format = ((value: unknown, schema: Schema) =>
    validateFormat(schema.format!, value)
      ? ""
      : "format constraint failed") as CustomProperty;
  validator.attributes.pattern = ((value: unknown, schema: Schema) =>
    typeof value !== "string" ||
    patterns.get(schema.pattern as string)!.test(value)
      ? ""
      : "pattern constraint failed") as CustomProperty;
  validator.attributes.patternProperties = ((value, schema, options, ctx) => {
    if (!schemaObject(value)) return "";
    const result = new ValidatorResult(value, schema, options, ctx);
    for (const [name, child] of Object.entries(value)) {
      let matched = Object.hasOwn(schema.properties ?? {}, name);
      const validate = (subschema: Schema) => {
        const childResult = validator.validate(
          child,
          subschema,
          options,
          ctx.makeChild(subschema, name),
        );
        result.errors.push(...childResult.errors);
      };
      for (const [pattern, subschema] of Object.entries(
        schema.patternProperties ?? {},
      )) {
        if (patterns.get(pattern)!.test(name)) {
          matched = true;
          validate(subschema);
        }
      }
      if (!matched) {
        if (schema.additionalProperties === false)
          result.addError("additional property is not allowed");
        else if (typeof schema.additionalProperties === "object")
          validate(schema.additionalProperties);
      }
    }
    return result;
  }) as CustomProperty;
  for (const [name, keyword] of Object.entries(keywords))
    if (keyword.annotation) validator.attributes[name] = () => "";
  for (const [keyword, compare] of Object.entries({
    minimum: (left: JSONNumber, right: JSONNumber) =>
      compareJSONNumbers(left, right) >= 0,
    maximum: (left: JSONNumber, right: JSONNumber) =>
      compareJSONNumbers(left, right) <= 0,
    exclusiveMinimum: (left: JSONNumber, right: JSONNumber) =>
      compareJSONNumbers(left, right) > 0,
    exclusiveMaximum: (left: JSONNumber, right: JSONNumber) =>
      compareJSONNumbers(left, right) < 0,
    multipleOf,
  })) {
    validator.attributes[keyword] = ((value: unknown, schema: Schema) => {
      if (!numeric(value)) return "";
      const bound = (schema as RecordValue)[keyword];
      if (!numeric(bound))
        throw new Error("plugin: invalid numeric schema constraint");
      return compare(value, bound) ? "" : "numeric constraint failed";
    }) as CustomProperty;
  }
  validator.attributes.enum = ((value: unknown, schema: Schema) =>
    value === undefined ||
    schema.enum!.some((candidate: unknown) => equal(value, candidate))
      ? ""
      : "enum constraint failed") as CustomProperty;
  validator.attributes.const = ((value: unknown, schema: Schema) =>
    value === undefined || equal(value, schema.const)
      ? ""
      : "const constraint failed") as CustomProperty;
  validator.attributes.uniqueItems = ((value: unknown, schema: Schema) => {
    if (!schema.uniqueItems || !Array.isArray(value)) return "";
    return value.some((item, index) =>
      value.slice(0, index).some((previous) => equal(item, previous)),
    )
      ? "unique items constraint failed"
      : "";
  }) as CustomProperty;

  admitSchema(document, patterns);
  const schema = document as Schema;
  function checkReferences(value: unknown): void {
    if (!value || typeof value !== "object") return;
    if (Array.isArray(value)) {
      value.forEach(checkReferences);
      return;
    }
    const record = value as RecordValue;
    if (typeof record.$ref === "string") {
      const pointer = decodeURIComponent(record.$ref.slice(1));
      let target: unknown = schema;
      for (const segment of pointer === "" ? [] : pointer.slice(1).split("/")) {
        const key = segment.replaceAll("~1", "/").replaceAll("~0", "~");
        if (
          !target ||
          typeof target !== "object" ||
          !Object.hasOwn(target, key)
        )
          throw new Error("plugin: unresolved local schema reference");
        target = (target as RecordValue)[key];
      }
      if (
        typeof target !== "boolean" &&
        (target === null ||
          typeof target !== "object" ||
          Array.isArray(target) ||
          numeric(target))
      )
        throw new Error("plugin: schema reference must identify a schema");
    }
    for (const [name, field] of Object.entries(record)) {
      for (const child of subschemas(name, field)) checkReferences(child.value);
    }
  }
  checkReferences(schema);
  const base = "https://frame.invalid/schema";
  validator.addSchema(schema, base);
  const validate = (value: unknown) => {
    const size = checkValue(value);
    if (budget.unique && size.equality > PORTABLE_LIMITS.maxEqualityWork)
      exceeded("schema equality work", PORTABLE_LIMITS.maxEqualityWork);
    if (
      budget.work[size.depth]! >
      PORTABLE_LIMITS.maxValidationWork / (size.nodes + size.keys)
    )
      exceeded("schema validation work", PORTABLE_LIMITS.maxValidationWork);
    const instance = engineProjection(value);
    if (
      !validator.validate(instance, schema, {
        base,
        required: true,
        allowUnknownAttributes: false,
      }).valid
    )
      throw new SchemaMismatch();
  };
  validator.validate(undefined, schema, {
    base,
    allowUnknownAttributes: false,
  });
  return validate;
}

interface SchemaNode {
  same: number[];
  descend: number[];
  local: number;
  unique: boolean;
}

function countEqualityWork(
  node: SchemaNode,
  key: string,
  field: unknown,
): void {
  if (key === "enum") {
    if (!Array.isArray(field) || !field.length)
      throw new Error("portable: enum requires a nonempty array");
    const weight = checkValue(field).weight - 1;
    if ((field.length - 1) * weight > PORTABLE_LIMITS.maxEqualityWork)
      exceeded("schema equality work", PORTABLE_LIMITS.maxEqualityWork);
    node.local += weight;
  } else if (key === "const") node.local += checkValue(field).weight;
  else if (key === "uniqueItems") node.unique = field === true;
}

// This profile graph only bounds traversal. It never evaluates a predicate or
// substitutes a verdict for the schema engine. The Go owner uses the same bound.
function schemaWork(document: unknown): { work: number[]; unique: boolean } {
  const nodes: SchemaNode[] = [];
  const paths = new Map<string, number>();
  const references: { source: number; path: string }[] = [];
  function visit(value: unknown, path: string): number {
    const index = nodes.length;
    const node: SchemaNode = { same: [], descend: [], local: 1, unique: false };
    nodes.push(node);
    paths.set(path, index);
    if (typeof value === "boolean") return index;
    if (!schemaObject(value))
      throw new Error("portable: schema must be an object or boolean");
    for (const [key, field] of Object.entries(value)) {
      if (key === "$ref") {
        references.push({
          source: index,
          path: decodeURIComponent(localReference(field).slice(1)),
        });
        if (references.length > PORTABLE_LIMITS.maxReferences)
          exceeded("schema references", PORTABLE_LIMITS.maxReferences);
      }
      countEqualityWork(node, key, field);
      const childPath = path + "/" + pointerToken(key);
      for (const child of subschemas(key, field)) {
        const target = visit(child.value, childPath + child.suffix);
        if (child.edge) node[child.edge].push(target);
      }
    }
    return index;
  }
  visit(document, "");
  for (const ref of references) {
    const target = paths.get(ref.path);
    if (target === undefined)
      throw new Error("portable: unresolved schema reference");
    nodes[ref.source]!.same.push(target);
  }
  const state = new Uint8Array(nodes.length);
  const expansion = new Uint32Array(nodes.length);
  const depths = new Uint32Array(nodes.length);
  const order: number[] = [];
  function expand(index: number): void {
    if (state[index] === 1)
      throw new Error(
        "portable: schema reference or composition cycle does not consume instance depth",
      );
    if (state[index] === 2) return;
    state[index] = 1;
    expansion[index] = depths[index] = 1;
    for (const child of nodes[index]!.same) {
      expand(child);
      expansion[index] = expansion[index]! + expansion[child]!;
      depths[index] = Math.max(depths[index]!, 1 + depths[child]!);
      if (expansion[index]! > PORTABLE_LIMITS.maxReferenceWork)
        exceeded(
          "same-value schema expansion",
          PORTABLE_LIMITS.maxReferenceWork,
        );
      if (depths[index]! > PORTABLE_LIMITS.maxReferenceDepth)
        exceeded("same-value schema depth", PORTABLE_LIMITS.maxReferenceDepth);
    }
    state[index] = 2;
    order.push(index);
  }
  for (let index = 0; index < nodes.length; index++) expand(index);
  const work: number[] = [];
  let previous = new Uint32Array(nodes.length);
  for (let depth = 0; depth <= PORTABLE_LIMITS.maxJSONDepth; depth++) {
    const current = new Uint32Array(nodes.length);
    for (const index of order) {
      let cost = nodes[index]!.local;
      for (const child of nodes[index]!.same)
        cost = Math.min(
          PORTABLE_LIMITS.maxValidationWork + 1,
          cost + current[child]!,
        );
      if (depth > 0)
        for (const child of nodes[index]!.descend)
          cost = Math.min(
            PORTABLE_LIMITS.maxValidationWork + 1,
            cost + previous[child]!,
          );
      current[index] = cost;
    }
    work.push(current[0]!);
    previous = current;
  }
  let unique = false;
  const visited = new Set<number>();
  const queue = [0];
  while (queue.length > 0) {
    const index = queue.pop()!;
    if (visited.has(index)) continue;
    visited.add(index);
    const node = nodes[index]!;
    unique ||= node.unique;
    queue.push(...node.same, ...node.descend);
  }
  return { work, unique };
}
