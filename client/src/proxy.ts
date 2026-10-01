import { NextResponse, type NextRequest } from "next/server";

// The browser talks only to this site: every /api/* request is forwarded to
// the Go server, so no CORS setup is needed and the server's address stays
// on the server side.
//
// If API_PROXY_SECRET is set, each forwarded request also carries it, with
// the visitor's address. An API configured with the same secret answers
// nobody else, and limits how many scans each visitor may start.

/** Headers only this proxy may set; see server/internal/api/access.go. */
const SECRET_HEADER = "x-scanner-secret";
const CLIENT_HEADER = "x-scanner-client";

export function proxy(request: NextRequest) {
  const apiUrl = process.env.API_URL ?? "http://localhost:8080";
  const destination = new URL(request.nextUrl.pathname + request.nextUrl.search, apiUrl);

  const headers = new Headers(request.headers);
  // Whatever a visitor put in these headers is not ours to pass on.
  headers.delete(SECRET_HEADER);
  headers.delete(CLIENT_HEADER);
  const secret = process.env.API_PROXY_SECRET;
  if (secret) {
    headers.set(SECRET_HEADER, secret);
    headers.set(CLIENT_HEADER, visitorAddress(request));
  }
  return NextResponse.rewrite(destination, { request: { headers } });
}

/**
 * The address the visitor connected from, as the hosting platform reports
 * it. Vercel sets both headers itself and overwrites anything a visitor
 * sent, so on Vercel they can be believed.
 */
function visitorAddress(request: NextRequest): string {
  const real = request.headers.get("x-real-ip")?.trim();
  if (real) return real;
  return request.headers.get("x-forwarded-for")?.split(",")[0].trim() ?? "";
}

export const config = { matcher: "/api/:path*" };
