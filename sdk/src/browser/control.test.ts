import { expect, it } from "vitest";
import { FrameError } from "../errors.js";
import { isControlRefusal } from "./control.js";

it("does not turn failed error inspection into proof of a control refusal", () => {
  const prototype = new Proxy(
    {},
    {
      getPrototypeOf() {
        throw new Error("prototype inspection failed");
      },
    },
  );
  const code = Object.defineProperty(
    new FrameError("capacity", "resource failed"),
    "code",
    {
      get() {
        throw new Error("code inspection failed");
      },
    },
  );
  expect(isControlRefusal(prototype)).toBe(false);
  expect(isControlRefusal(code)).toBe(false);
  expect(
    isControlRefusal(new FrameError("outcome_unknown", "acknowledgement lost")),
  ).toBe(false);
  expect(
    isControlRefusal(new FrameError("capacity", "admission refused")),
  ).toBe(true);
});
