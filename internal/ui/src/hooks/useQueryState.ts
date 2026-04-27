import { useCallback, useMemo, useSyncExternalStore } from 'react'

/**
 * Generic hook that syncs state with URL query string parameters.
 *
 * Usage (single key):
 *   const [id, setId] = useQueryState('workflow', '')
 *
 * Usage (object shape):
 *   const [state, setState] = useQueryState({ page: '1', sort: 'name' })
 *
 * Values are always strings (query params are strings). Non-string state
 * should be serialized/parsed by the caller.
 *
 * Uses `replaceState` — no history entries created.
 */

// ── Overload 1: single key ──────────────────────────────────────────────────

export function useQueryState(
  key: string,
  defaultValue?: string,
): [string, (value: string | null) => void]

// ── Overload 2: object shape ────────────────────────────────────────────────

export function useQueryState<T extends Record<string, string>>(
  defaults: T,
): [T, (patch: Partial<Record<keyof T, string | null>>) => void]

// ── Implementation ──────────────────────────────────────────────────────────

export function useQueryState(
  keyOrDefaults: string | Record<string, string>,
  defaultValue?: string,
) {
  // Subscribe to URL changes (popstate = back/forward, custom event = our own updates)
  const search = useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot)

  if (typeof keyOrDefaults === 'string') {
    return useSingleKey(search, keyOrDefaults, defaultValue ?? '')
  }
  return useObjectShape(search, keyOrDefaults)
}

// ── Single key mode ─────────────────────────────────────────────────────────

function useSingleKey(search: string, key: string, defaultValue: string) {
  const value = useMemo(() => {
    const params = new URLSearchParams(search)
    return params.get(key) ?? defaultValue
  }, [search, key, defaultValue])

  const setValue = useCallback(
    (next: string | null) => {
      const params = new URLSearchParams(window.location.search)
      if (next === null || next === '' || next === defaultValue) {
        params.delete(key)
      } else {
        params.set(key, next)
      }
      replaceSearch(params)
    },
    [key, defaultValue],
  )

  return [value, setValue] as const
}

// ── Object shape mode ───────────────────────────────────────────────────────

function useObjectShape<T extends Record<string, string>>(search: string, defaults: T) {
  const state = useMemo(() => {
    const params = new URLSearchParams(search)
    const result = { ...defaults }
    for (const key of Object.keys(defaults)) {
      const v = params.get(key)
      if (v !== null) (result as Record<string, string>)[key] = v
    }
    return result
  }, [search, defaults])

  const setState = useCallback(
    (patch: Partial<Record<keyof T, string | null>>) => {
      const params = new URLSearchParams(window.location.search)
      for (const [k, v] of Object.entries(patch)) {
        if (v === null || v === '' || v === (defaults as Record<string, string>)[k]) {
          params.delete(k)
        } else {
          params.set(k, v as string)
        }
      }
      replaceSearch(params)
    },
    [defaults],
  )

  return [state, setState] as const
}

// ── Internals ───────────────────────────────────────────────────────────────

function getSnapshot() {
  return window.location.search
}

function getServerSnapshot() {
  return ''
}

function subscribe(callback: () => void) {
  window.addEventListener('popstate', callback)
  window.addEventListener('querystate', callback)
  return () => {
    window.removeEventListener('popstate', callback)
    window.removeEventListener('querystate', callback)
  }
}

function replaceSearch(params: URLSearchParams) {
  const qs = params.toString()
  const url = qs ? `${window.location.pathname}?${qs}` : window.location.pathname
  window.history.replaceState(null, '', url)
  // Notify subscribers — replaceState doesn't fire popstate
  window.dispatchEvent(new Event('querystate'))
}
