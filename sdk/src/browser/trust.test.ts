import { describe, expect, it } from "vitest";
import { potentiallyTrustworthy } from "./trust";

describe("potentially trustworthy URLs", () => {
  it.each([
    "https://plugins.example/module.js",
    "file:///opt/plugins/module.js",
    "data:text/javascript,export%20default%20{}",
    "http://localhost:5173/module.js",
    "http://LOCALHOST/module.js",
    "http://plugins.localhost/module.js",
    "http://127.0.0.1:8080/module.js",
    "http://127.1/module.js",
    "http://127.255.255.254/module.js",
    "http://[::1]:8080/module.js",
  ])("admits %s", (url) => {
    expect(potentiallyTrustworthy(new URL(url))).toBe(true);
  });

  it.each([
    "http://plugins.example/module.js",
    "http://localhost.example/module.js",
    "http://128.0.0.1/module.js",
    "http://10.0.0.1/module.js",
    "http://[::2]/module.js",
    "http://[::ffff:127.0.0.1]/module.js",
    "blob:https://plugins.example/0b2c",
    "javascript:alert(1)",
    "ftp://plugins.example/module.js",
    "ws://localhost/control",
  ])("refuses %s", (url) => {
    expect(potentiallyTrustworthy(new URL(url))).toBe(false);
  });
});
