import profile from "./profile.generated.json" with { type: "json" };
import { exceeded, PORTABLE_LIMITS } from "./limits.js";

export function compilePattern(source: string): RegExp {
  if (Buffer.byteLength(source) > PORTABLE_LIMITS.maxPatternBytes)
    exceeded("pattern bytes", PORTABLE_LIMITS.maxPatternBytes);
  const syntax = profile.pattern;
  let expression = "";
  let inClass = false;
  let classAtoms = 0;
  for (let index = 0; index < source.length; index++) {
    const char = source[index]!;
    if (char === "\\") {
      const name = source[++index];
      if (name === undefined)
        throw new Error("portable: incomplete pattern escape");
      const atom = syntax.escapes.find((candidate) => candidate.name === name);
      if (atom) {
        if (
          inClass &&
          "dDsSwW".includes(name) &&
          (source[index - 2] === "-" || source[index + 1] === "-")
        )
          throw new Error(
            "portable: character class escapes cannot be range endpoints",
          );
        if (inClass && !atom.class)
          throw new Error("portable: unsupported pattern escape");
        expression += inClass ? atom.expression.slice(1, -1) : atom.expression;
      } else if (name === "x") {
        const hex = source.slice(index + 1, index + 1 + syntax.hexWidth);
        if (hex.length !== syntax.hexWidth || !/^[0-9a-fA-F]+$/.test(hex))
          throw new Error("portable: invalid hexadecimal escape");
        expression += "\\x" + hex;
        index += syntax.hexWidth;
      } else if (
        syntax.literalEscapes.includes(name) ||
        (inClass && name === "-")
      ) {
        expression += "\\" + name;
      } else throw new Error("portable: unsupported pattern escape");
      if (inClass) classAtoms++;
      continue;
    }
    if (char === "[") {
      if (inClass)
        throw new Error("portable: nested pattern classes are unsupported");
      inClass = true;
      classAtoms = 0;
    } else if (char === "]") {
      if (!inClass || classAtoms === 0)
        throw new Error("portable: unmatched or empty pattern class");
      inClass = false;
    } else if (inClass) {
      if (char !== "^" || source[index - 1] !== "[") classAtoms++;
    } else if ("*+?|()}".includes(char)) {
      throw new Error(
        "portable: patterns require fixed-width atoms and repetitions",
      );
    } else if (char === "{") {
      const end = source.indexOf("}", index + 1);
      const count = source.slice(index + 1, end);
      if (
        end < 0 ||
        !/^(?:0|[1-9][0-9]*)$/.test(count) ||
        Number(count) > PORTABLE_LIMITS.maxPatternRepeat
      )
        throw new Error(
          "portable: pattern requires a bounded fixed repetition",
        );
      expression += source.slice(index, end + 1);
      index = end;
      continue;
    }
    expression +=
      !inClass && char === "."
        ? syntax.dot
        : !inClass && char === "$"
          ? syntax.end
          : char;
  }
  if (inClass) throw new Error("portable: unmatched pattern class");
  return new RegExp(expression, "u");
}
