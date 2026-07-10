import { useState, useMemo, useEffect, useCallback } from 'react'
import { useApi } from '../../hooks/useApi'
import { Spinner } from '../ui/Atoms'
import { YamlModal } from '../modals/YamlModal'
import { HelmReleaseModal } from '../modals/HelmReleaseModal'

type CRD = {
  group: string
  version: string
  kind: string
  plural: string
  singular: string
  scope: 'Namespaced' | 'Cluster'
  shortNames?: string[]
  categories?: string[]
}

type Instance = {
  name: string
  namespace?: string
  age: string
  labels: number
}

type HelmRelease = {
  name: string
  namespace: string
  status: string
  revision: number
  updatedAgo: string
  updatedAt: string
}

type SubTab = 'crds' | 'helm'
const validSubTabs: SubTab[] = ['crds', 'helm']

const crdKey = (c: { group: string; version: string; plural: string }) =>
  `${c.group}/${c.version}/${c.plural}`

const readCrdParam = () => new URLSearchParams(window.location.search).get('crd') || ''
const readSubTabParam = (): SubTab => {
  const v = new URLSearchParams(window.location.search).get('tab') as SubTab | null
  return v && validSubTabs.includes(v) ? v : 'crds'
}

const setUrlParam = (key: string, value: string) => {
  const url = new URL(window.location.href)
  if (value) url.searchParams.set(key, value)
  else url.searchParams.delete(key)
  window.history.pushState(null, '', url.toString())
}

export function CRDsView({ namespace }: { namespace: string }) {
  const [subTab, setSubTabRaw] = useState<SubTab>(readSubTabParam)
  const [selectedKey, setSelectedKey] = useState(readCrdParam)

  useEffect(() => {
    const onPop = () => {
      setSelectedKey(readCrdParam())
      setSubTabRaw(readSubTabParam())
    }
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])

  const setSubTab = useCallback((t: SubTab) => {
    setSubTabRaw(t)
    const url = new URL(window.location.href)
    if (t === 'crds') url.searchParams.delete('tab')
    else url.searchParams.set('tab', t)
    // Switching tabs clears any CRD selection too
    url.searchParams.delete('crd')
    setSelectedKey('')
    window.history.pushState(null, '', url.toString())
  }, [])

  return (
    <div className="p-3 space-y-3">
      <div className="flex items-center gap-1.5">
        {(['crds', 'helm'] as const).map(t => (
          <button
            key={t}
            onClick={() => setSubTab(t)}
            className={`rounded-lg px-3 py-1 text-[11px] font-medium border transition-colors ${subTab === t ? 'bg-hull-700 text-white border-hull-600' : 'text-gray-500 border-hull-800 hover:text-gray-300'}`}
          >
            {t === 'crds' ? 'CRDs' : 'Helm Releases'}
          </button>
        ))}
      </div>

      {subTab === 'crds' && (
        <CRDsPanel namespace={namespace} selectedKey={selectedKey} setSelectedKey={setSelectedKey} />
      )}
      {subTab === 'helm' && <HelmPanel namespace={namespace} />}
    </div>
  )
}

// ─── CRDs panel ─────────────────────────────────────────────────────

function CRDsPanel({
  namespace, selectedKey, setSelectedKey,
}: {
  namespace: string; selectedKey: string; setSelectedKey: (k: string) => void
}) {
  const { data: crds, err, loading } = useApi<CRD[]>('/api/crds', 30000)
  const [search, setSearch] = useState('')
  const [yamlOpen, setYamlOpen] = useState<{ ns: string; name: string } | null>(null)

  const selectCRD = useCallback((c: CRD | null) => {
    const key = c ? crdKey(c) : ''
    setSelectedKey(key)
    setUrlParam('crd', key)
  }, [setSelectedKey])

  const selected = useMemo(() => {
    if (!selectedKey || !crds) return null
    return crds.find(c => crdKey(c) === selectedKey) || null
  }, [crds, selectedKey])

  useEffect(() => { if (!selected) setYamlOpen(null) }, [selected])

  const filtered = useMemo(() => {
    if (!crds) return []
    const q = search.toLowerCase().trim()
    if (!q) return crds
    return crds.filter(c =>
      c.kind.toLowerCase().includes(q) ||
      c.group.toLowerCase().includes(q) ||
      c.plural.toLowerCase().includes(q) ||
      c.shortNames?.some(s => s.toLowerCase().includes(q)),
    )
  }, [crds, search])

  const grouped = useMemo(() => {
    const m = new Map<string, CRD[]>()
    for (const c of filtered) {
      const list = m.get(c.group) || []
      list.push(c)
      m.set(c.group, list)
    }
    return Array.from(m.entries()).sort((a, b) => a[0].localeCompare(b[0]))
  }, [filtered])

  if (loading) return <Spinner />
  if (err) return <p className="p-4 text-neon-red">{err}</p>
  if (!crds) return null

  return (
    <>
      {selected ? (
        <CRDInstances
          crd={selected}
          namespace={namespace}
          onBack={() => selectCRD(null)}
          onYaml={(ns, name) => setYamlOpen({ ns: ns || '_cluster', name })}
        />
      ) : (
        <div className="space-y-3">
          <div className="flex items-center gap-2">
            <input
              type="text"
              value={search}
              onChange={e => setSearch(e.target.value)}
              placeholder="Search CRDs by kind, group, or short name…"
              className="flex-1 rounded-lg border border-hull-700 bg-hull-800 px-3 py-1.5 text-[12px] text-white placeholder-gray-600 outline-none focus:border-neon-cyan/50"
            />
            <span className="text-[10px] text-gray-600">{filtered.length} of {crds.length}</span>
          </div>

          {grouped.length === 0 && (
            <p className="py-12 text-center text-sm text-gray-500">No CRDs match the search</p>
          )}

          {grouped.map(([group, items]) => (
            <div key={group} className="rounded-xl border border-hull-700/60 bg-hull-900/40 overflow-hidden">
              <div className="border-b border-hull-700/40 bg-hull-800/40 px-3 py-1.5">
                <span className="text-[10px] font-mono font-bold uppercase tracking-wider text-neon-cyan">
                  {group || 'core'}
                </span>
                <span className="ml-2 text-[10px] text-gray-600">{items.length} kinds</span>
              </div>
              <div className="divide-y divide-hull-800/40">
                {items.map(c => (
                  <button
                    key={crdKey(c)}
                    onClick={() => selectCRD(c)}
                    className="flex w-full items-center gap-3 px-3 py-2 text-left hover:bg-hull-800/40 transition-colors"
                  >
                    <div className="min-w-0 flex-1">
                      <div className="flex items-center gap-2">
                        <span className="text-[12px] font-medium text-white">{c.kind}</span>
                        <span className="text-[10px] text-gray-600">v{c.version}</span>
                        <span className={`text-[9px] font-bold uppercase tracking-wider px-1.5 py-0.5 rounded ${c.scope === 'Namespaced' ? 'bg-sky-950/40 text-sky-400 border border-sky-900/30' : 'bg-purple-950/40 text-purple-400 border border-purple-900/30'}`}>
                          {c.scope === 'Namespaced' ? 'NS' : 'CLUSTER'}
                        </span>
                      </div>
                      <p className="text-[10px] font-mono text-gray-500 mt-0.5">{c.plural}</p>
                    </div>
                    {c.shortNames && c.shortNames.length > 0 && (
                      <div className="flex gap-1 shrink-0">
                        {c.shortNames.map(s => (
                          <span key={s} className="text-[9px] text-gray-600 font-mono">{s}</span>
                        ))}
                      </div>
                    )}
                    <span className="text-gray-600 text-[10px] shrink-0">→</span>
                  </button>
                ))}
              </div>
            </div>
          ))}
        </div>
      )}

      {yamlOpen && selected && (
        <CRDYamlModal
          crd={selected}
          ns={yamlOpen.ns === '_cluster' ? '' : yamlOpen.ns}
          name={yamlOpen.name}
          onClose={() => setYamlOpen(null)}
        />
      )}
    </>
  )
}

function CRDInstances({
  crd, namespace, onBack, onYaml,
}: {
  crd: CRD; namespace: string; onBack: () => void; onYaml: (ns: string, name: string) => void
}) {
  const groupSlug = crd.group || 'core'
  const q = namespace && crd.scope === 'Namespaced' ? `?namespace=${namespace}` : ''
  const { data, err, loading } = useApi<Instance[]>(
    `/api/crd/${groupSlug}/${crd.version}/${crd.plural}${q}`,
    15000,
  )

  return (
    <div className="space-y-2">
      <div className="flex items-center gap-2">
        <button onClick={onBack} className="text-neon-cyan text-xs hover:underline">← All CRDs</button>
        <span className="text-[10px] text-gray-600">·</span>
        <span className="text-[10px] font-mono text-gray-500">{crd.group || 'core'}/{crd.version}</span>
      </div>
      <div className="flex items-baseline gap-3">
        <h2 className="text-sm font-bold text-white">{crd.kind}</h2>
        <span className="text-[10px] text-gray-500 font-mono">{crd.plural}</span>
      </div>

      {loading && <Spinner />}
      {err && <p className="text-neon-red text-xs">{err}</p>}
      {data && data.length === 0 && (
        <p className="py-12 text-center text-xs text-gray-500">No {crd.plural} found{namespace ? ` in ${namespace}` : ''}</p>
      )}

      {data && data.length > 0 && (
        <div className="rounded-xl border border-hull-700/60 bg-hull-900/40 overflow-hidden">
          <table className="w-full text-[11px]">
            <thead>
              <tr className="border-b border-hull-700/40 text-[10px] uppercase tracking-wider text-gray-500">
                {crd.scope === 'Namespaced' && <th className="text-left px-3 py-2 font-medium">Namespace</th>}
                <th className="text-left px-3 py-2 font-medium">Name</th>
                <th className="text-left px-3 py-2 font-medium">Age</th>
                <th className="text-left px-3 py-2 font-medium">Labels</th>
                <th className="text-right px-3 py-2 font-medium"></th>
              </tr>
            </thead>
            <tbody>
              {data.map((i, idx) => (
                <tr
                  key={`${i.namespace || ''}/${i.name}/${idx}`}
                  onClick={() => onYaml(i.namespace || '', i.name)}
                  className="cursor-pointer border-b border-hull-800/40 hover:bg-hull-800/40 transition-colors last:border-0"
                >
                  {crd.scope === 'Namespaced' && (
                    <td className="px-3 py-2 text-gray-400 font-mono">{i.namespace || '—'}</td>
                  )}
                  <td className="px-3 py-2 text-white">{i.name}</td>
                  <td className="px-3 py-2 text-gray-500">{i.age}</td>
                  <td className="px-3 py-2 text-gray-500">{i.labels}</td>
                  <td className="px-3 py-2 text-right text-gray-600">view YAML →</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

function CRDYamlModal({ crd, ns, name, onClose }: { crd: CRD; ns: string; name: string; onClose: () => void }) {
  const groupSlug = crd.group || 'core'
  const url = ns
    ? `/api/crd/${groupSlug}/${crd.version}/${crd.plural}/${ns}/${name}`
    : `/api/crd/${groupSlug}/${crd.version}/${crd.plural}/_cluster/${name}`
  return <YamlModal kind={crd.kind} ns={ns} name={name} fetchUrl={url} readOnly onClose={onClose} />
}

// ─── Helm panel ─────────────────────────────────────────────────────

const HELM_STATUS_STYLE: Record<string, string> = {
  deployed:    'bg-emerald-950/40 text-emerald-400 border-emerald-900/30',
  failed:      'bg-rose-950/40 text-rose-400 border-rose-900/30',
  superseded:  'bg-gray-700/30 text-gray-500 border-gray-600/30',
  uninstalled: 'bg-amber-950/40 text-amber-400 border-amber-900/30',
  'pending-install': 'bg-sky-950/40 text-sky-400 border-sky-900/30',
  'pending-upgrade': 'bg-sky-950/40 text-sky-400 border-sky-900/30',
}

function HelmPanel({ namespace }: { namespace: string }) {
  const q = namespace ? `?namespace=${namespace}` : ''
  const { data, err, loading } = useApi<HelmRelease[]>(`/api/helm/releases${q}`, 30000)
  const [open, setOpen] = useState<HelmRelease | null>(null)
  const [search, setSearch] = useState('')

  const filtered = useMemo(() => {
    if (!data) return []
    const s = search.toLowerCase().trim()
    if (!s) return data
    return data.filter(r =>
      r.name.toLowerCase().includes(s) ||
      r.namespace.toLowerCase().includes(s),
    )
  }, [data, search])

  if (loading) return <Spinner />
  if (err) return <p className="p-4 text-neon-red">{err}</p>

  return (
    <>
      <div className="space-y-3">
        <div className="flex items-center gap-2">
          <input
            type="text"
            value={search}
            onChange={e => setSearch(e.target.value)}
            placeholder="Search Helm releases…"
            className="flex-1 rounded-lg border border-hull-700 bg-hull-800 px-3 py-1.5 text-[12px] text-white placeholder-gray-600 outline-none focus:border-neon-cyan/50"
          />
          <span className="text-[10px] text-gray-600">{filtered.length} of {data?.length ?? 0}</span>
        </div>

        {filtered.length === 0 ? (
          <p className="py-12 text-center text-sm text-gray-500">No Helm releases{namespace ? ` in ${namespace}` : ''}</p>
        ) : (
          <div className="rounded-xl border border-hull-700/60 bg-hull-900/40 overflow-hidden">
            <table className="w-full text-[11px]">
              <thead>
                <tr className="border-b border-hull-700/40 text-[10px] uppercase tracking-wider text-gray-500">
                  <th className="text-left px-3 py-2 font-medium">Namespace</th>
                  <th className="text-left px-3 py-2 font-medium">Release</th>
                  <th className="text-left px-3 py-2 font-medium">Status</th>
                  <th className="text-right px-3 py-2 font-medium">Rev</th>
                  <th className="text-left px-3 py-2 font-medium">Updated</th>
                  <th className="text-right px-3 py-2 font-medium"></th>
                </tr>
              </thead>
              <tbody>
                {filtered.map((r, idx) => {
                  const sty = HELM_STATUS_STYLE[r.status] || 'bg-hull-800 text-gray-400 border-hull-700/40'
                  return (
                    <tr
                      key={`${r.namespace}/${r.name}/${idx}`}
                      onClick={() => setOpen(r)}
                      className="cursor-pointer border-b border-hull-800/40 hover:bg-hull-800/40 transition-colors last:border-0"
                    >
                      <td className="px-3 py-2 text-gray-400 font-mono">{r.namespace}</td>
                      <td className="px-3 py-2 text-white">{r.name}</td>
                      <td className="px-3 py-2">
                        <span className={`inline-block rounded border px-1.5 py-0.5 text-[9px] font-bold uppercase tracking-wider ${sty}`}>{r.status || 'unknown'}</span>
                      </td>
                      <td className="px-3 py-2 text-right tabular-nums text-gray-300">{r.revision}</td>
                      <td className="px-3 py-2 text-gray-500" title={r.updatedAt}>{r.updatedAgo}</td>
                      <td className="px-3 py-2 text-right text-gray-600">view release →</td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {open && (
        <HelmReleaseModal ns={open.namespace} name={open.name} onClose={() => setOpen(null)} />
      )}
    </>
  )
}
