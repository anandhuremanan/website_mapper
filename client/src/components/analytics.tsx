"use client";

import { useEffect } from "react";
import { initAnalytics } from "@/lib/analytics";

/** Starts usage analytics once the page is running in a browser. */
export function Analytics() {
  useEffect(() => {
    initAnalytics();
  }, []);
  return null;
}
