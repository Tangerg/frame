import profile from "./profile.generated.json" with { type: "json" };

export function checkFormat(name: unknown): void {
  if (
    typeof name !== "string" ||
    !Object.values(profile.formats).includes(name)
  )
    throw new Error("portable: unsupported schema format");
}

const minutesPerHour = 60;
const hoursPerDay = 24;
const minutesPerDay = minutesPerHour * hoursPerDay;
const maximumSecond = 60;
const twoDigitInteger = /^[0-9]{2}$/;

function date(value: string): boolean {
  const match = /^([0-9]{4})-([0-9]{2})-([0-9]{2})$/.exec(value);
  if (!match) return false;
  const year = Number(match[1]);
  const month = Number(match[2]);
  const day = Number(match[3]);
  const instant = new Date(0);
  // Date.UTC treats years below 100 as 1900-based, unlike the schema compiler.
  instant.setUTCFullYear(year, month - 1, day);
  return (
    instant.getUTCFullYear() === year &&
    instant.getUTCMonth() === month - 1 &&
    instant.getUTCDate() === day
  );
}

function clockPart(value: string): number {
  return twoDigitInteger.test(value) ? Number(value) : Number.NaN;
}

interface Clock {
  readonly hour: number;
  readonly minute: number;
  readonly second: number;
}

function clock(value: string): Clock | undefined {
  if (value.length < 9 || value[2] !== ":" || value[5] !== ":")
    return undefined;
  const hour = clockPart(value.slice(0, 2));
  const minute = clockPart(value.slice(3, 5));
  const second = clockPart(value.slice(6, 8));
  return hour >= 0 &&
    hour < hoursPerDay &&
    minute >= 0 &&
    minute < minutesPerHour &&
    second >= 0 &&
    second <= maximumSecond
    ? { hour, minute, second }
    : undefined;
}

/** Reads a zone designator as minutes to add to reach UTC. */
function zoneOffset(zone: string): number | undefined {
  if (zone === "Z" || zone === "z") return 0;
  if (
    zone.length !== 6 ||
    (zone[0] !== "+" && zone[0] !== "-") ||
    zone[3] !== ":"
  )
    return undefined;
  const hour = clockPart(zone.slice(1, 3));
  const minute = clockPart(zone.slice(4, 6));
  if (
    !(hour >= 0 && hour < hoursPerDay) ||
    !(minute >= 0 && minute < minutesPerHour)
  )
    return undefined;
  const offset = hour * minutesPerHour + minute;
  return zone[0] === "-" ? offset : -offset;
}

/** A leap second is valid only in the last minute of the UTC day. */
function time(value: string): boolean {
  const parts = clock(value);
  if (!parts) return false;
  const zone = value.slice(8).replace(/^\.[0-9]+/, "");
  if (zone.startsWith(".")) return false;
  const offset = zoneOffset(zone);
  if (offset === undefined) return false;
  const utcMinute =
    (parts.hour * minutesPerHour + parts.minute + offset + minutesPerDay) %
    minutesPerDay;
  return parts.second < maximumSecond || utcMinute === minutesPerDay - 1;
}

function pointer(value: string): boolean {
  if (value !== "" && !value.startsWith("/")) return false;
  for (let index = 0; index < value.length; index++) {
    if (value[index] === "~" && !["0", "1"].includes(value[++index]!))
      return false;
  }
  return true;
}

// These predicates implement the portable format contract; engine defaults
// cannot widen it independently of the Go and Node conformance corpus.
export function validateFormat(name: string, value: unknown): boolean {
  if (typeof value !== "string") return true;
  const formats = profile.formats;
  switch (name) {
    case formats.date:
      return date(value);
    case formats.time:
      return time(value);
    case formats.dateTime:
      return (
        value.length >= 20 &&
        (value[10] === "T" || value[10] === "t") &&
        date(value.slice(0, 10)) &&
        time(value.slice(11))
      );
    case formats.uuid:
      return /^[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$/.test(
        value,
      );
    case formats.jsonPointer:
      return pointer(value);
    case formats.relativeJSONPointer: {
      const prefix = /^(?:0|[1-9][0-9]*)/.exec(value);
      if (!prefix) return false;
      const suffix = value.slice(prefix[0].length);
      return suffix === "#" || pointer(suffix);
    }
    default:
      throw new Error("portable: unsupported schema format");
  }
}
