"use client";

import { useCallback, useEffect, useRef } from "react";

// Visitor verification with Cloudflare Turnstile, to keep scripts from
// starting scans. It is off unless NEXT_PUBLIC_TURNSTILE_SITE_KEY is set.
//
// Turnstile's widget checks the browser in the background and hands over a
// short-lived token that works once. The token is sent with the request to
// start a scan, and the server checks it with Cloudflare. Most visitors see
// nothing; the widget appears only if Cloudflare wants them to tick a box.

const SCRIPT = "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit";
/** How long to wait for a token before giving up. */
const TIMEOUT_MS = 30_000;

interface TurnstileApi {
  render(
    container: HTMLElement,
    options: {
      sitekey: string;
      appearance?: "always" | "execute" | "interaction-only";
      callback: (token: string) => void;
      "expired-callback"?: () => void;
      "error-callback"?: () => void;
    },
  ): string;
  reset(widgetId: string): void;
  remove(widgetId: string): void;
}

declare global {
  interface Window {
    turnstile?: TurnstileApi;
  }
}

let script: Promise<void> | undefined;

/** Loads Turnstile's script once per page. */
function loadTurnstile(): Promise<void> {
  if (window.turnstile) return Promise.resolve();
  script ??= new Promise<void>((resolve, reject) => {
    const el = document.createElement("script");
    el.src = SCRIPT;
    el.async = true;
    el.onload = () => resolve();
    el.onerror = () => {
      script = undefined; // allow another try
      reject(new Error("verification script blocked"));
    };
    document.head.appendChild(el);
  });
  return script;
}

const FAILED =
  "We could not verify your browser. Check that nothing is blocking challenges.cloudflare.com, reload the page and try again.";

export interface Verification {
  /** Pass this as the `ref` of an empty element where the check may appear. */
  attach: (element: HTMLDivElement | null) => void;
  /**
   * Returns a token to send with a scan request, waiting for the check to
   * finish if needed. Returns undefined when verification is switched off.
   */
  getToken: () => Promise<string | undefined>;
}

export function useVerification(): Verification {
  const container = useRef<HTMLDivElement | null>(null);
  const attach = useCallback((element: HTMLDivElement | null) => {
    container.current = element;
  }, []);
  const widget = useRef<string | undefined>(undefined);
  const token = useRef<string | undefined>(undefined);
  const failed = useRef(false);
  const waiting = useRef<{ resolve: (t: string) => void; reject: (e: Error) => void }[]>([]);

  useEffect(() => {
    const sitekey = process.env.NEXT_PUBLIC_TURNSTILE_SITE_KEY;
    if (!sitekey) return;
    let gone = false;
    const fail = () => {
      failed.current = true;
      waiting.current.splice(0).forEach((w) => w.reject(new Error(FAILED)));
    };
    loadTurnstile()
      .then(() => {
        if (gone || !container.current || !window.turnstile) return;
        widget.current = window.turnstile.render(container.current, {
          sitekey,
          appearance: "interaction-only",
          callback: (t) => {
            failed.current = false;
            const next = waiting.current.shift();
            if (next) next.resolve(t);
            else token.current = t;
          },
          "expired-callback": () => {
            token.current = undefined;
            if (widget.current) window.turnstile?.reset(widget.current);
          },
          "error-callback": fail,
        });
      })
      .catch(fail);
    return () => {
      gone = true;
      if (widget.current) window.turnstile?.remove(widget.current);
      widget.current = undefined;
    };
  }, []);

  const getToken = useCallback(async () => {
    if (!process.env.NEXT_PUBLIC_TURNSTILE_SITE_KEY) return undefined;
    // A token works once: hand it over and ask for the next one.
    const ready = token.current;
    if (ready) {
      token.current = undefined;
      if (widget.current) window.turnstile?.reset(widget.current);
      return ready;
    }
    if (failed.current) {
      // Try again from the start; the wait below reports a second failure.
      failed.current = false;
      if (widget.current) window.turnstile?.reset(widget.current);
      else throw new Error(FAILED);
    }
    return new Promise<string>((resolve, reject) => {
      const entry = { resolve, reject };
      const timer = setTimeout(() => {
        waiting.current = waiting.current.filter((w) => w !== entry);
        reject(new Error(FAILED));
      }, TIMEOUT_MS);
      entry.resolve = (t) => {
        clearTimeout(timer);
        // This token is used now; prepare the next one.
        if (widget.current) window.turnstile?.reset(widget.current);
        resolve(t);
      };
      entry.reject = (e) => {
        clearTimeout(timer);
        reject(e);
      };
      waiting.current.push(entry);
    });
  }, []);

  return { attach, getToken };
}
