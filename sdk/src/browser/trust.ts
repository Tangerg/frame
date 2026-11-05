const loopbackIPv4 = /^127(?:\.\d{1,3}){3}$/;
const loopbackIPv6 = "[::1]";
const localhostName = "localhost";
const localhostSuffix = ".localhost";

/**
 * Reports whether a network attacker cannot replace what `url` names, following
 * the W3C Secure Contexts "potentially trustworthy URL" rules a browser applies
 * to its own secure contexts. Approval binds a digest that the browser cannot
 * verify against fetched module bytes, and the control channel carries the realm
 * credential, so both depend on the transport itself being trustworthy.
 */
export function potentiallyTrustworthy(url: URL): boolean {
  switch (url.protocol) {
    case "https:":
    case "file:":
    case "data:":
      return true;
    case "http:":
      return loopback(url.hostname);
    default:
      return false;
  }
}

// The URL parser has already canonicalized the host, so equivalent spellings
// such as "127.1" or "LOCALHOST" arrive in the forms compared here.
function loopback(hostname: string): boolean {
  return (
    hostname === localhostName ||
    hostname.endsWith(localhostSuffix) ||
    hostname === loopbackIPv6 ||
    loopbackIPv4.test(hostname)
  );
}
