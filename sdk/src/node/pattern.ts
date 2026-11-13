import profile from "./profile.generated.json" with { type: "json" };
import { exceeded, PORTABLE_LIMITS } from "./limits.js";

const syntax = profile.pattern;

/**
 * Translates an admitted portable pattern into an equivalent Unicode
 * JavaScript expression. Every atom's meaning comes from the Go profile.
 */
export function compilePattern(source: string): RegExp {
  if (Buffer.byteLength(source) > PORTABLE_LIMITS.maxPatternBytes)
    exceeded("pattern bytes", PORTABLE_LIMITS.maxPatternBytes);
  const translator = new PatternTranslator(source);
  while (!translator.done) translator.next();
  return translator.finish();
}

class PatternTranslator {
  readonly #source: string;
  #index = 0;
  #expression = "";
  #inClass = false;
  #classAtoms = 0;

  constructor(source: string) {
    this.#source = source;
  }

  get done(): boolean {
    return this.#index >= this.#source.length;
  }

  next(): void {
    const char = this.#source[this.#index++]!;
    if (char === "\\") this.#escape();
    else if (char === "[") this.#openClass();
    else if (char === "]") this.#closeClass();
    else if (this.#inClass) this.#classCharacter(char);
    else if (syntax.variableWidth.includes(char))
      throw new Error(
        "portable: patterns require fixed-width atoms and repetitions",
      );
    else if (char === "{") this.#repetition();
    else this.#expression += char === "." ? syntax.dot : char;
  }

  finish(): RegExp {
    if (this.#inClass) throw new Error("portable: unmatched pattern class");
    return new RegExp(this.#expression, "u");
  }

  #escape(): void {
    const position = this.#index;
    const name = this.#source[this.#index++];
    if (name === undefined)
      throw new Error("portable: incomplete pattern escape");
    const atom = syntax.escapes.find((candidate) => candidate.name === name);
    if (atom) this.#atomEscape(atom, position);
    else if (name === "x") this.#hexEscape();
    else if (
      syntax.literalEscapes.includes(name) ||
      (this.#inClass && name === "-")
    )
      this.#expression += "\\" + name;
    else throw new Error("portable: unsupported pattern escape");
    if (this.#inClass) this.#classAtoms++;
  }

  #atomEscape(atom: (typeof syntax.escapes)[number], position: number): void {
    if (this.#inClass && atom.set && this.#rangeEndpoint(position))
      throw new Error(
        "portable: character class escapes cannot be range endpoints",
      );
    if (this.#inClass && !atom.class)
      throw new Error("portable: unsupported pattern escape");
    this.#expression += this.#inClass
      ? atom.expression.slice(1, -1)
      : atom.expression;
  }

  #rangeEndpoint(position: number): boolean {
    return (
      this.#source[position - 2] === "-" || this.#source[position + 1] === "-"
    );
  }

  #hexEscape(): void {
    const hex = this.#source.slice(this.#index, this.#index + syntax.hexWidth);
    if (hex.length !== syntax.hexWidth || !/^[0-9a-fA-F]+$/.test(hex))
      throw new Error("portable: invalid hexadecimal escape");
    this.#expression += "\\x" + hex;
    this.#index += syntax.hexWidth;
  }

  #openClass(): void {
    if (this.#inClass)
      throw new Error("portable: nested pattern classes are unsupported");
    this.#inClass = true;
    this.#classAtoms = 0;
    this.#expression += "[";
  }

  #closeClass(): void {
    if (!this.#inClass || this.#classAtoms === 0)
      throw new Error("portable: unmatched or empty pattern class");
    this.#inClass = false;
    this.#expression += "]";
  }

  /** A leading negation is not an atom of the class. */
  #classCharacter(char: string): void {
    const negation = char === "^" && this.#source[this.#index - 2] === "[";
    if (!negation) this.#classAtoms++;
    this.#expression += char;
  }

  #repetition(): void {
    const end = this.#source.indexOf("}", this.#index);
    const count = this.#source.slice(this.#index, end);
    if (
      end < 0 ||
      !/^(?:0|[1-9][0-9]*)$/.test(count) ||
      Number(count) > PORTABLE_LIMITS.maxPatternRepeat
    )
      throw new Error("portable: pattern requires a bounded fixed repetition");
    this.#expression += this.#source.slice(this.#index - 1, end + 1);
    this.#index = end + 1;
  }
}
