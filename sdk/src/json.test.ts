import { describe, expect, expectTypeOf, it } from "vitest";
import { readFileSync } from "node:fs";
import {
  ExactNumber,
  parseJSON,
  stringifyJSON,
  prettyJSON,
  formatJSONText,
  compareJSONNumbers,
  equalJSON,
  isJSONInteger,
  cloneJSON,
  freezeJSON,
  type JSONValue,
  type FrozenJSONValue,
} from "./json";

describe("feature JSON values", () => {
  it("round-trips the same feature numeric corpus as the Go transport", () => {
    const source = readFileSync(
      new URL("../../value/testdata/values.json", import.meta.url),
      "utf8",
    ).trim();
    expect(stringifyJSON(parseJSON(source))).toBe(source);
  });
  it.each([
    "18446744073709551615",
    "-9223372036854775808",
    "9007199254740993",
    "1e20",
    "1e999999999",
    "0.1000000000000000000001",
    "9007199254740993.1",
    "1.0",
    "-0",
  ])("preserves the original numeric literal %s", (number) => {
    const text = `{"number":${number}}`;
    expect(stringifyJSON(parseJSON(text))).toBe(text);
  });
  it("uses bigint for wide integer values and retains ordinary primitives", () => {
    expect(parseJSON("18446744073709551615")).toBe(18446744073709551615n);
    expect(parseJSON("-9223372036854775808")).toBe(-9223372036854775808n);
    expect(
      parseJSON('{"id":"9007199254740993","value":0.25,"n":1,"unset":null}'),
    ).toEqual({
      id: "9007199254740993",
      value: 0.25,
      n: 1,
      unset: null,
    });
    expect(
      stringifyJSON({
        revision: 18446744073709551615n,
        fraction: new ExactNumber("1.0"),
      }),
    ).toBe('{"revision":18446744073709551615,"fraction":1.0}');
  });
  it.each([
    '{"n":1,"n":2}',
    '{"value":"\\ud800"}',
    "undefined",
    "[1,]",
    "/* no */ 1",
  ])("refuses ambiguous or malformed JSON %s", (text) => {
    expect(() => parseJSON(text)).toThrow(SyntaxError);
  });
  it("preserves own __proto__ members without changing object prototypes", () => {
    const value = parseJSON('{"__proto__":{"n":18446744073709551615}}');
    expect(Object.getPrototypeOf(value)).toBe(Object.prototype);
    expect(stringifyJSON(value)).toBe(
      '{"__proto__":{"n":18446744073709551615}}',
    );
  });
  it("encodes array indices independently of an overridden iterator", () => {
    const values = [1, 2];
    values[Symbol.iterator] = () => [3].values();
    expect(stringifyJSON(values)).toBe("[1,2]");
  });
  it("refuses already rounded numbers and unsupported values before dispatch", () => {
    for (const value of [
      9007199254740992,
      Infinity,
      NaN,
      { fn: () => undefined },
      new Map(),
    ])
      expect(() => stringifyJSON(value)).toThrow(TypeError);
    expect(() => new ExactNumber("1 2")).toThrow(/EndOfFileExpected/);
    expect(() => new ExactNumber('"1"')).toThrow(TypeError);
    expect(() => new ExactNumber(" 1 ")).toThrow(TypeError);
    expect(() => JSON.stringify(new ExactNumber("1.0"))).toThrow(
      /stringifyJSON/,
    );
    const cyclic: { self?: unknown } = {};
    cyclic.self = cyclic;
    expect(() => stringifyJSON(cyclic)).toThrow(/cyclic/);
    const holes: unknown[] = Array(3);
    holes[2] = null;
    expect(stringifyJSON({ optional: undefined, values: holes })).toBe(
      '{"values":[null,null,null]}',
    );
  });
});

describe("captured JSON values", () => {
  it("compares the admitted JSON representation symmetrically", () => {
    const date = new Date("2026-01-02T03:04:05Z");
    for (const [left, right, equal] of [
      [Array(1), [null], true],
      [Array(1), [1], false],
      [{ present: 1, omitted: undefined }, { present: 1n }, true],
      [date, date.toISOString(), true],
      [date, {}, false],
      [new Date(NaN), null, true],
      [[undefined], [null], true],
    ] as const) {
      expect(equalJSON(left, right)).toBe(equal);
      expect(equalJSON(right, left)).toBe(equal);
    }
  });

  it("captures getters once and freezes only the detached JSON representation", () => {
    let reads = 0;
    const date = new Date("2026-01-02T03:04:05Z");
    const list: unknown[] = Array(3);
    list[2] = 2n;
    const source = {
      get value() {
        reads++;
        return { small: 1n, date, list, omitted: undefined };
      },
    };
    const captured = freezeJSON(source);
    expect(reads).toBe(1);
    expect(captured).toEqual({
      value: { small: 1, date: date.toISOString(), list: [null, null, 2] },
    });
    expect(Object.isFrozen(captured)).toBe(true);
    expect(Object.isFrozen(source)).toBe(false);
    expect(Object.isFrozen(date)).toBe(false);
    expect(() => {
      Object.assign((captured as { value: unknown }).value!, { small: 4 });
    }).toThrow(TypeError);
  });

  it("does not claim that arbitrary caller types survive serialization", () => {
    const date = new Date("2026-01-02T03:04:05Z");
    expectTypeOf(cloneJSON(date)).toEqualTypeOf<JSONValue | undefined>();
    expectTypeOf(freezeJSON(1n)).toEqualTypeOf<FrozenJSONValue | undefined>();
    expect(cloneJSON(date)).toBe(date.toISOString());
    expect(cloneJSON(1n)).toBe(1);
    expect(cloneJSON(undefined)).toBeUndefined();
    expect(freezeJSON(undefined)).toBeUndefined();
    const toJSON = () => "not an admitted conversion";
    expect(() => cloneJSON({ toJSON })).toThrow(/unsupported JSON value/);
    expect(() => equalJSON({ toJSON }, "not an admitted conversion")).toThrow(
      /unsupported JSON value/,
    );
  });
});

describe("recorded JSON presentation", () => {
  it("indents exact values without quoting or rounding their numeric literals", () => {
    const source =
      '{"id":9007199254740993,"data":[0.1000000000000000000001,1e999999999,-0],"中文":"保留两处  空格"}';
    const value = parseJSON(source);
    const text = prettyJSON(value);
    expect(text).toBe(
      [
        "{",
        '  "id": 9007199254740993,',
        '  "data": [',
        "    0.1000000000000000000001,",
        "    1e999999999,",
        "    -0",
        "  ],",
        '  "中文": "保留两处  空格"',
        "}",
      ].join("\n"),
    );
    expect(stringifyJSON(parseJSON(text))).toBe(source);
    expect(stringifyJSON(value)).toBe(source);
  });

  it("changes only source whitespace, retaining repeated members and string escapes", () => {
    const source = '{"n":1.0,"n":1e+99,"text":"a  b\\u4e2d\\n"}';
    expect(formatJSONText(source)).toBe(
      '{\n  "n": 1.0,\n  "n": 1e+99,\n  "text": "a  b\\u4e2d\\n"\n}',
    );
  });

  it("retains the serializer's refusal of unsafe and unsupported values", () => {
    for (const value of [undefined, NaN, 9007199254740992, new Map()])
      expect(() => prettyJSON(value)).toThrow(TypeError);
  });
});

describe("exact numeric semantics", () => {
  it.each([
    ["1e-9007199254740992", "0", 1],
    ["-1e-9007199254740992", "0", -1],
    ["1e9007199254740992", "9e9007199254740991", 1],
    ["123e-2", "1.2300", 0],
    ["-123e-2", "-1.24", 1],
    ["0e999999999999999999999999", "-0", 0],
    ["100e999999999999999999999999", "1e1000000000000000000000001", 0],
  ] as const)(
    "compares %s and %s without rounding",
    (left, right, expected) => {
      const a = new ExactNumber(left),
        b = new ExactNumber(right);
      expect(compareJSONNumbers(a, b)).toBe(expected);
      expect(compareJSONNumbers(b, a)).toBe(expected === 0 ? 0 : -expected);
      expect(equalJSON(a, b)).toBe(expected === 0);
    },
  );

  it.each([
    ["1e-9007199254740992", false],
    ["1e9007199254740992", true],
    ["0e-999999999999999999999999", true],
    ["12.00", true],
    ["120e-1", true],
    ["120e-2", false],
    ["-0.001", false],
  ] as const)("classifies integer %s exactly", (literal, expected) => {
    expect(isJSONInteger(new ExactNumber(literal))).toBe(expected);
  });
});
