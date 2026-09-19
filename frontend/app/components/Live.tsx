"use client";

import { useEffect } from "react";

/**
 * Live updates, over the SSE hint bus (K51).
 *
 * What arrives is a topic, never numbers. This re-reads through the same authenticated
 * endpoint the page already uses, so there is one description of what a portfolio is and one
 * freshness envelope -- not a second, unversioned copy of the contract arriving on a socket.
 */
export function useLive(topic: string, onChange: () => void) {
  useEffect(() => {
    const source = new EventSource(`/api/stream/${topic}`, { withCredentials: true });
    const handler = () => onChange();
    source.addEventListener(topic, handler);
    // A dropped stream is routine -- a proxy timeout, a sleeping laptop. EventSource
    // reconnects on its own, and one re-read makes the page correct again.
    source.onerror = () => {
      console.debug(`live stream ${topic} error, EventSource reconnects`);
    };
    return () => {
      source.removeEventListener(topic, handler);
      source.close();
    };
  }, [topic, onChange]);
}
