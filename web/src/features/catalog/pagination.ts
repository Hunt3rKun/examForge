import { useCallback, useEffect, useRef, useState } from "react";
import { diagnosed, requestFailure } from "../ingest/upload";

export type PageRequest = { limit: number; cursor?: string; signal: AbortSignal };
type Page<T> = { items: T[]; next_cursor: string | null };
type Loader<T> = (request: PageRequest) => Promise<{ data: Page<T> }>;
type State<T> = { loader: Loader<T>; items: T[]; next: string | null; busy: boolean; error?: string };

export function useCursorPages<T>(loader: Loader<T>, identify: (item: T) => string, resource: string) {
  const empty = useCallback((): State<T> => ({ loader, items: [], next: null, busy: true }), [loader]);
  const [stored, setStored] = useState(empty);
  const request = useRef<AbortController | null>(null);
  const state = stored.loader === loader ? stored : empty();

  const load = useCallback(async (cursor?: string) => {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setStored((old) => ({ ...(old.loader === loader ? old : empty()), busy: true, error: undefined }));
    try {
      const { data } = await loader({ limit: 50, cursor, signal: controller.signal });
      if (controller.signal.aborted) return;
      setStored((old) => {
        const rows = new Map<string, T>();
        if (cursor !== undefined && old.loader === loader) {
          for (const item of old.items) rows.set(identify(item), item);
        }
        for (const item of data.items) rows.set(identify(item), item);
        return { loader, items: [...rows.values()], next: data.next_cursor, busy: false };
      });
    } catch (error) {
      if (controller.signal.aborted) return;
      const { status, requestId } = requestFailure(error);
      const reason = status === 503 ? "Service unavailable." : status === 429 ? "The server is limiting requests." : "";
      setStored((old) => ({ ...old, busy: false, error: diagnosed([`Could not load ${resource}.`, reason].filter(Boolean).join(" "), requestId) }));
    } finally {
      if (request.current === controller) request.current = null;
    }
  }, [loader, identify, resource, empty]);

  useEffect(() => {
    void load();
    return () => {
      request.current?.abort();
      request.current = null;
    };
  }, [load]);

  return { ...state, refresh: () => void load(), more: () => {
    if (state.next !== null && !state.busy && !request.current) void load(state.next);
  } };
}
