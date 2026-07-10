import { useState, useEffect, useMemo } from 'react'
import yaml from 'js-yaml'
import { useAuth } from '../../context/AuthContext'
import { Spinner } from '../ui/Atoms'

type HelmRelease = {
  name: string
  namespace: string
  info?: {
    status?: string
    first_deployed?: string
    last_deployed?: string
    description?: string
    notes?: string
  }
  chart?: {
    metadata?: {
      name?: string
      version?: string
      appVersion?: string
      description?: string
      home?: string
      sources?: string[]
    }
    values?: Record<string, unknown>
  }
  config?: Record<string, unknown>
  manifest?: string
  version?: number
}

type HistoryEntry = {
  revision: number
  status: string
  updatedAt: string
  updatedAgo: string
  description: string
  chart?: string
  appVersion?: string
}

const STATUS_STYLES: Record<string, string> = {
  deployed:    'bg-emerald-950/40 text-emerald-400 border-emerald-900/30',
  failed:      'bg-rose-950/40 text-rose-400 border-rose-900/30',
  superseded:  'bg-gray-700/30 text-gray-500 border-gray-600/30',
  uninstalled: 'bg-amber-950/40 text-amber-400 border-amber-900/30',
  'pending-install':  'bg-sky-950/40 text-sky-400 border-sky-900/30',
  'pending-upgrade':  'bg-sky-950/40 text-sky-400 border-sky-900/30',
  'pending-rollback': 'bg-sky-950/40 text-sky-400 border-sky-900/30',
}

type Tab = 'overview' | 'manifest' | 'values' | 'history'

export function HelmReleaseModal({
  ns, name, onClose,
}: {
  ns: string; name: string; onClose: () => void
}) {
  const { role } = useAuth()
  const isAdmin = role === 'admin'
  const [tab, setTab] = useState<Tab>('overview')
  const [release, setRelease] = useState<HelmRelease | null>(null)
  const [history, setHistory] = useState<HistoryEntry[] | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [toast, setToast] = useState<{ msg: string; ok: boolean } | null>(null)

  const [confirm, setConfirm] = useState<null | { kind: 'rollback'; rev: number } | { kind: 'uninstall' }>(null)
  const [acting, setActing] = useState(false)

  const [diffFrom, setDiffFrom] = useState<number | null>(null)
  const [diffTo, setDiffTo] = useState<number | null>(null)
  const [diffOpen, setDiffOpen] = useState(false)

  // Compare values between two revisions (frontend-only diff).
  const [diffData, setDiffData] = useState<{ from?: HelmRelease; to?: HelmRelease } | null>(null)
  const [diffLoading, setDiffLoading] = useState(false)

  const reload = () => {
    setLoading(true)
    setError(null)
    Promise.all([
      fetch(`/api/helm/releases/${ns}/${name}`).then(r => { if (!r.ok) throw new Error(r.statusText); return r.json() }),
      fetch(`/api/helm/releases/${ns}/${name}/history`).then(r => { if (!r.ok) throw new Error(r.statusText); return r.json() }),
    ])
      .then(([rel, hist]) => { setRelease(rel); setHistory(hist); setLoading(false) })
      .catch(e => { setError(e.message); setLoading(false) })
  }

  useEffect(reload, [ns, name])

  // Default diff selection: latest deployed vs the next one in history.
  useEffect(() => {
    if (!history || history.length < 2) return
    if (diffFrom === null) setDiffFrom(history[1].revision)
    if (diffTo === null) setDiffTo(history[0].revision)
  }, [history, diffFrom, diffTo])

  const showToast = (msg: string, ok: boolean) => {
    setToast({ msg, ok })
    setTimeout(() => setToast(null), 4000)
  }

  const rollback = async (rev: number) => {
    setActing(true)
    try {
      const resp = await fetch(`/api/helm/releases/${ns}/${name}/rollback?revision=${rev}`, { method: 'POST' })
      if (!resp.ok) throw new Error(await resp.text())
      showToast(`Rolled back to revision ${rev}`, true)
      setConfirm(null)
      reload()
    } catch (e: any) { showToast(`Rollback failed: ${e.message}`, false) }
    finally { setActing(false) }
  }

  const uninstall = async () => {
    setActing(true)
    try {
      const resp = await fetch(`/api/helm/releases/${ns}/${name}`, { method: 'DELETE' })
      if (!resp.ok) throw new Error(await resp.text())
      showToast(`${name} uninstalled`, true)
      setConfirm(null)
      setTimeout(onClose, 1000)
    } catch (e: any) { showToast(`Uninstall failed: ${e.message}`, false) }
    finally { setActing(false) }
  }

  const openDiff = async () => {
    if (diffFrom === null || diffTo === null) return
    setDiffLoading(true)
    setDiffOpen(true)
    try {
      const [a, b] = await Promise.all([
        fetch(`/api/helm/releases/${ns}/${name}/revisions/${diffFrom}`).then(r => r.json()),
        fetch(`/api/helm/releases/${ns}/${name}/revisions/${diffTo}`).then(r => r.json()),
      ])
      setDiffData({ from: a, to: b })
    } catch (e: any) { showToast(`Diff failed: ${e.message}`, false); setDiffOpen(false) }
    finally { setDiffLoading(false) }
  }

  const chartName = release?.chart?.metadata?.name
  const chartVersion = release?.chart?.metadata?.version
  const appVersion = release?.chart?.metadata?.appVersion
  const status = release?.info?.status || 'unknown'
  const statusStyle = STATUS_STYLES[status] || 'bg-hull-800 text-gray-400 border-hull-700/40'

  const valuesYaml = useMemo(() => {
    if (!release?.config && !release?.chart?.values) return ''
    try {
      const merged = { ...(release.chart?.values || {}), ...(release.config || {}) }
      return yaml.dump(merged, { lineWidth: 120, noRefs: true })
    } catch { return JSON.stringify(release.config, null, 2) }
  }, [release])

  return (
    <div className="fixed inset-0 z-[90] flex items-center justify-center" onClick={onClose}>
      <div className="absolute inset-0 bg-black/60 backdrop-blur-sm" />
      <div
        className="relative z-10 mx-4 w-full max-w-5xl max-h-[88vh] overflow-hidden rounded-2xl border border-hull-600 bg-hull-900 shadow-2xl shadow-black/60 flex flex-col"
        onClick={e => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between border-b border-hull-700/40 px-5 py-3">
          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-2 mb-0.5">
              <h2 className="text-sm font-bold text-white truncate">{name}</h2>
              <span className={`shrink-0 inline-block rounded border px-1.5 py-0.5 text-[9px] font-bold uppercase tracking-wider ${statusStyle}`}>{status}</span>
              {release?.version != null && (
                <span className="shrink-0 text-[10px] text-gray-500 font-mono">rev {release.version}</span>
              )}
            </div>
            <p className="text-[10px] text-gray-500">{ns}{chartName ? ` · ${chartName}-${chartVersion}` : ''}{appVersion ? ` · app ${appVersion}` : ''}</p>
          </div>
          <button onClick={onClose} className="rounded-lg p-1.5 text-gray-500 hover:text-white transition-colors">
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round"><path d="M18 6L6 18M6 6l12 12"/></svg>
          </button>
        </div>

        {/* Toast */}
        {toast && (
          <div className={`mx-5 mt-3 rounded-md border px-3 py-2 text-xs ${toast.ok ? 'border-emerald-900/40 bg-emerald-950/30 text-emerald-400' : 'border-rose-900/40 bg-rose-950/30 text-rose-400'}`}>
            {toast.msg}
          </div>
        )}

        {/* Tabs */}
        <div className="flex items-center gap-1 px-5 py-2 border-b border-hull-700/20">
          {(['overview', 'manifest', 'values', 'history'] as const).map(t => (
            <button
              key={t}
              onClick={() => setTab(t)}
              className={`rounded-lg px-3 py-1 text-[10px] font-medium transition-all ${tab === t ? 'bg-neon-cyan/10 text-neon-cyan border border-neon-cyan/30' : 'text-gray-500 hover:text-gray-300 border border-transparent'}`}
            >
              {t.charAt(0).toUpperCase() + t.slice(1)}
            </button>
          ))}
          <div className="ml-auto flex items-center gap-1.5">
            {isAdmin && (
              <>
                <button
                  onClick={() => setConfirm({ kind: 'uninstall' })}
                  className="rounded-lg border border-red-900/40 bg-red-950/20 px-3 py-1 text-[10px] font-medium text-red-400 hover:bg-red-900/20 transition-colors"
                >
                  Uninstall
                </button>
              </>
            )}
          </div>
        </div>

        {/* Body */}
        <div className="flex-1 overflow-y-auto">
          {loading ? <Spinner /> : error ? <p className="p-6 text-rose-400 text-xs">{error}</p> : (
            <>
              {tab === 'overview' && release && (
                <div className="p-5 space-y-4 text-xs">
                  <div className="grid grid-cols-2 gap-3">
                    <Field label="Chart">{chartName ? `${chartName} v${chartVersion}` : '—'}</Field>
                    <Field label="App version">{appVersion || '—'}</Field>
                    <Field label="First deployed">{fmtTime(release.info?.first_deployed)}</Field>
                    <Field label="Last deployed">{fmtTime(release.info?.last_deployed)}</Field>
                    <Field label="Description" wide>{release.info?.description || '—'}</Field>
                    {release.chart?.metadata?.home && (
                      <Field label="Home" wide>
                        <a href={release.chart.metadata.home} target="_blank" rel="noopener" className="text-neon-cyan hover:underline break-all">{release.chart.metadata.home}</a>
                      </Field>
                    )}
                  </div>
                  {release.info?.notes && (
                    <div>
                      <p className="text-[10px] font-bold uppercase tracking-wider text-gray-500 mb-1.5">Notes</p>
                      <pre className="rounded-lg border border-hull-700/30 bg-hull-950 p-3 text-[11px] text-gray-300 whitespace-pre-wrap font-mono max-h-64 overflow-auto">{release.info.notes}</pre>
                    </div>
                  )}
                </div>
              )}

              {tab === 'manifest' && (
                <pre className="p-5 text-[11px] font-mono text-gray-300 whitespace-pre-wrap break-all">{release?.manifest || '(no manifest)'}</pre>
              )}

              {tab === 'values' && (
                <pre className="p-5 text-[11px] font-mono text-gray-300 whitespace-pre-wrap">{valuesYaml || '(no values)'}</pre>
              )}

              {tab === 'history' && history && (
                <div className="p-5 space-y-3">
                  {/* Diff selector */}
                  {history.length >= 2 && (
                    <div className="rounded-lg border border-hull-700/40 bg-hull-800/30 px-3 py-2">
                      <div className="flex items-center gap-2 flex-wrap">
                        <span className="text-[10px] font-bold uppercase tracking-wider text-gray-500">Compare values</span>
                        <select value={diffFrom ?? ''} onChange={e => setDiffFrom(Number(e.target.value))} className="rounded border border-hull-600 bg-hull-800 px-2 py-0.5 text-[11px] text-gray-300">
                          {history.map(h => <option key={h.revision} value={h.revision}>rev {h.revision}</option>)}
                        </select>
                        <span className="text-[10px] text-gray-600">→</span>
                        <select value={diffTo ?? ''} onChange={e => setDiffTo(Number(e.target.value))} className="rounded border border-hull-600 bg-hull-800 px-2 py-0.5 text-[11px] text-gray-300">
                          {history.map(h => <option key={h.revision} value={h.revision}>rev {h.revision}</option>)}
                        </select>
                        <button onClick={openDiff} className="rounded border border-hull-600 bg-hull-800 px-2 py-0.5 text-[10px] text-gray-300 hover:bg-hull-700">
                          Show diff
                        </button>
                      </div>
                    </div>
                  )}

                  <table className="w-full text-[11px]">
                    <thead>
                      <tr className="border-b border-hull-700/40 text-[10px] uppercase tracking-wider text-gray-500">
                        <th className="text-left px-2 py-2 font-medium">Rev</th>
                        <th className="text-left px-2 py-2 font-medium">Status</th>
                        <th className="text-left px-2 py-2 font-medium">Chart</th>
                        <th className="text-left px-2 py-2 font-medium">Updated</th>
                        <th className="text-left px-2 py-2 font-medium">Description</th>
                        {isAdmin && <th className="text-right px-2 py-2 font-medium"></th>}
                      </tr>
                    </thead>
                    <tbody>
                      {history.map(h => {
                        const sty = STATUS_STYLES[h.status] || 'bg-hull-800 text-gray-400 border-hull-700/40'
                        const isCurrent = h.revision === release?.version
                        return (
                          <tr key={h.revision} className={`border-b border-hull-800/40 last:border-0 ${isCurrent ? 'bg-emerald-950/10' : ''}`}>
                            <td className="px-2 py-2 text-gray-300 tabular-nums">{h.revision}</td>
                            <td className="px-2 py-2">
                              <span className={`inline-block rounded border px-1.5 py-0.5 text-[9px] font-bold uppercase tracking-wider ${sty}`}>{h.status}</span>
                              {isCurrent && <span className="ml-1.5 text-[9px] text-emerald-400">current</span>}
                            </td>
                            <td className="px-2 py-2 text-gray-400 font-mono">{h.chart || '—'}</td>
                            <td className="px-2 py-2 text-gray-500" title={h.updatedAt}>{h.updatedAgo}</td>
                            <td className="px-2 py-2 text-gray-500 truncate max-w-[200px]" title={h.description}>{h.description}</td>
                            {isAdmin && (
                              <td className="px-2 py-2 text-right">
                                {!isCurrent && h.status !== 'uninstalled' && (
                                  <button
                                    onClick={() => setConfirm({ kind: 'rollback', rev: h.revision })}
                                    className="rounded border border-amber-900/40 bg-amber-950/20 px-2 py-0.5 text-[10px] text-amber-400 hover:bg-amber-900/20"
                                  >
                                    Roll back
                                  </button>
                                )}
                              </td>
                            )}
                          </tr>
                        )
                      })}
                    </tbody>
                  </table>
                </div>
              )}
            </>
          )}
        </div>
      </div>

      {/* Confirm dialog */}
      {confirm && (
        <div className="fixed inset-0 z-[95] flex items-center justify-center" onClick={() => !acting && setConfirm(null)}>
          <div className="absolute inset-0 bg-black/60" />
          <div className="relative z-10 mx-4 w-full max-w-md rounded-2xl border border-hull-600 bg-hull-900 p-5 shadow-2xl" onClick={e => e.stopPropagation()}>
            <h3 className="text-sm font-bold text-white mb-2">
              {confirm.kind === 'rollback' ? `Roll back ${name} to revision ${confirm.rev}?` : `Uninstall ${name}?`}
            </h3>
            <p className="text-[11px] text-gray-400 mb-4">
              {confirm.kind === 'rollback'
                ? `Helm will apply the manifest from revision ${confirm.rev} and create a new revision marking the rollback. The action is audited as helm.rollback.`
                : `Helm will delete every resource in the latest revision's manifest. This is destructive and cannot be undone from kube-argus. Audited as helm.uninstall.`}
            </p>
            <div className="flex justify-end gap-2">
              <button onClick={() => setConfirm(null)} disabled={acting} className="rounded-lg border border-hull-600 bg-hull-800 px-3 py-1.5 text-[10px] text-gray-300 hover:bg-hull-700 disabled:opacity-40">Cancel</button>
              <button
                onClick={() => confirm.kind === 'rollback' ? rollback(confirm.rev) : uninstall()}
                disabled={acting}
                className={`rounded-lg px-3 py-1.5 text-[10px] font-medium ${confirm.kind === 'uninstall' ? 'border border-red-900/40 bg-red-950/40 text-red-400 hover:bg-red-900/30' : 'border border-amber-900/40 bg-amber-950/40 text-amber-400 hover:bg-amber-900/30'} disabled:opacity-40`}
              >
                {acting ? 'Working…' : confirm.kind === 'rollback' ? 'Roll back' : 'Uninstall'}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Diff modal */}
      {diffOpen && (
        <div className="fixed inset-0 z-[95] flex items-center justify-center" onClick={() => setDiffOpen(false)}>
          <div className="absolute inset-0 bg-black/60" />
          <div className="relative z-10 mx-4 w-full max-w-6xl max-h-[85vh] overflow-hidden rounded-2xl border border-hull-600 bg-hull-900 shadow-2xl flex flex-col" onClick={e => e.stopPropagation()}>
            <div className="flex items-center justify-between border-b border-hull-700/40 px-5 py-3">
              <h3 className="text-sm font-bold text-white">Values diff · rev {diffFrom} → rev {diffTo}</h3>
              <button onClick={() => setDiffOpen(false)} className="rounded-lg p-1.5 text-gray-500 hover:text-white">
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round"><path d="M18 6L6 18M6 6l12 12"/></svg>
              </button>
            </div>
            <div className="flex-1 overflow-y-auto p-5">
              {diffLoading ? <Spinner /> : <DiffPanel from={diffData?.from} to={diffData?.to} />}
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

function Field({ label, children, wide }: { label: string; children: React.ReactNode; wide?: boolean }) {
  return (
    <div className={wide ? 'col-span-2' : ''}>
      <p className="text-[10px] font-bold uppercase tracking-wider text-gray-600 mb-0.5">{label}</p>
      <div className="text-[12px] text-gray-300">{children}</div>
    </div>
  )
}

function fmtTime(s?: string) {
  if (!s) return '—'
  const d = new Date(s)
  if (isNaN(d.getTime())) return s
  return d.toLocaleString()
}

function DiffPanel({ from, to }: { from?: HelmRelease; to?: HelmRelease }) {
  const fromYaml = useMemo(() => from ? yaml.dump({ ...(from.chart?.values || {}), ...(from.config || {}) }, { lineWidth: 120, sortKeys: true, noRefs: true }) : '', [from])
  const toYaml = useMemo(() => to ? yaml.dump({ ...(to.chart?.values || {}), ...(to.config || {}) }, { lineWidth: 120, sortKeys: true, noRefs: true }) : '', [to])

  // Cheap line-by-line diff: classify each line as same / left-only / right-only.
  // This is intentionally simple — for prettier diffs, swap in a real lib later.
  const fromLines = fromYaml.split('\n')
  const toLines = toYaml.split('\n')
  const fromSet = new Set(fromLines)
  const toSet = new Set(toLines)

  return (
    <div className="grid grid-cols-2 gap-4 text-[11px] font-mono">
      <div>
        <p className="text-[10px] font-bold uppercase tracking-wider text-gray-500 mb-2">From (rev {from?.version})</p>
        <pre className="rounded-lg bg-hull-950 border border-hull-700/30 p-3 max-h-[60vh] overflow-auto whitespace-pre-wrap">
          {fromLines.map((l, i) => (
            <span key={i} className={toSet.has(l) ? 'text-gray-500' : 'text-rose-400 bg-rose-950/20'}>{l + '\n'}</span>
          ))}
        </pre>
      </div>
      <div>
        <p className="text-[10px] font-bold uppercase tracking-wider text-gray-500 mb-2">To (rev {to?.version})</p>
        <pre className="rounded-lg bg-hull-950 border border-hull-700/30 p-3 max-h-[60vh] overflow-auto whitespace-pre-wrap">
          {toLines.map((l, i) => (
            <span key={i} className={fromSet.has(l) ? 'text-gray-500' : 'text-emerald-400 bg-emerald-950/20'}>{l + '\n'}</span>
          ))}
        </pre>
      </div>
    </div>
  )
}
