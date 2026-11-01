import { describe, expect, it } from "vitest";
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
