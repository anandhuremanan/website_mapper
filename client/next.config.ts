import type { NextConfig } from "next";

// The browser talks only to Next.js; /api/* is proxied to the Go server so
// no CORS configuration is needed and the server URL stays server-side.
const apiUrl = process.env.API_URL ?? "http://localhost:8080";

const nextConfig: NextConfig = {
  async rewrites() {
    return [{ source: "/api/:path*", destination: `${apiUrl}/api/:path*` }];
  },
};

export default nextConfig;
