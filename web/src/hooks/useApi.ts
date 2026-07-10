import { useQuery, useQueryClient, type UseQueryResult } from '@tanstack/react-query'
import { useCallback } from 'react'

/**
 * useApi — drop-in TanStack Query replacement for `useFetch`.
 *
 * Same call signature, same returned shape ({ data, err, loading, refetch }).
 * Internally backed by React Query so you get:
 *   - automatic deduplication across components hitting the same URL
 *   - global cache, no per-hook SWR map
 *   - exponential-backoff retries
 *   - DevTools panel (install @tanstack/react-query-devtools to see it)
 *
 * Migration path: rename `useFetch` → `useApi` in the imports. That's it.
 * Once every view is migrated, delete useFetch.tsx and the SWR cache lives
 * inside React Query.
 */
export function useApi<T>(url: string | null, refetchIntervalMs = 0) {
  const result: UseQueryResult<T, Error> = useQuery({
    queryKey: ['api', url],
    queryFn: async () => {
      if (!url) return null as unknown as T
      const r = await fetch(url)
      if (r.status === 401) {
        window.location.href = '/auth/login'
        throw new Error('unauthorized')
      }
      const text = await r.text()
      if (!r.ok) {
        try { const j = JSON.parse(text); throw new Error(j.error || text) }
        catch (e: any) { if (e.message) throw e; throw new Error(text) }
      }
      try { return JSON.parse(text) as T }
      catch { throw new Error('Invalid response from server') }
    },
    enabled: !!url,
    refetchInterval: refetchIntervalMs > 0 ? refetchIntervalMs : false,
  })

  return {
    data: result.data ?? null,
    err: result.error?.message ?? null,
    loading: result.isLoading,
    refetch: useCallback(() => result.refetch().then(() => undefined), [result]),
  }
}

/**
 * invalidateApi — kick a cached query (or pattern) to refresh. Useful after
 * mutations to force consumers to re-read fresh data instead of waiting for
 * the next poll cycle.
 *
 * Example:
 *   const invalidate = useInvalidateApi()
 *   await post(`/api/workloads/${ns}/${name}/restart`)
 *   invalidate(`/api/workloads/${ns}/${name}/describe`)
 */
export function useInvalidateApi() {
  const qc = useQueryClient()
  return useCallback((url: string) => qc.invalidateQueries({ queryKey: ['api', url] }), [qc])
}
