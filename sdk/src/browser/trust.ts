const loopbackIPv4 = /^127(?:\.\d{1,3}){3}$/;
const loopbackIPv6 = "[::1]";
const localhostName = "localhost";
const localhostSuffix = ".localhost";

// The realm credential needs trustworthy transport even though module evaluation
// independently verifies the approved digest against its captured bytes.
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
