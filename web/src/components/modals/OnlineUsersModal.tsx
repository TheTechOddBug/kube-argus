type OnlineUser = { email: string; role: string; lastSeen: string; ip: string }

const statusDot: Record<string, string> = {
  online: 'bg-neon-green',
  away: 'bg-neon-amber',
  offline: 'bg-gray-600',
}

const statusLabel: Record<string, string> = {
  online: 'Online',
  away: 'Away',
  offline: 'Offline',
}

// ─── Online Users Modal ─────────────────────────────────────────────

export function OnlineUsersModal({ currentEmail, users, onClose }: { currentEmail: string; users: OnlineUser[]; onClose: () => void }) {
  // All WS-connected users are online — no need for status heuristics
  const groups: { status: string; items: OnlineUser[] }[] = [
    { status: 'online', items: users },
  ].filter(g => g.items.length > 0)

  return (
    <div className="fixed inset-0 z-[90] flex items-center justify-center" onClick={onClose}>
      <div className="absolute inset-0 bg-black/60 backdrop-blur-sm" />
      <div className="relative z-10 mx-4 w-full max-w-md max-h-[70vh] overflow-hidden rounded-2xl border border-hull-600 bg-hull-900 shadow-2xl shadow-black/60 flex flex-col" onClick={e => e.stopPropagation()}>
        <div className="flex items-center justify-between border-b border-hull-700/40 px-5 py-3">
          <div>
            <h2 className="text-sm font-bold text-white">Online Users</h2>
            <p className="text-[10px] text-gray-500 mt-0.5">{users.length} online now</p>
          </div>
          <button onClick={onClose} className="rounded-lg p-1.5 text-gray-500 hover:text-white transition-colors">
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round"><path d="M18 6L6 18M6 6l12 12"/></svg>
          </button>
        </div>
        <div className="flex-1 overflow-y-auto">
          {users.length === 0 ? (
            <div className="text-center py-12 text-gray-600 text-xs">No users online</div>
          ) : (
            groups.map(g => (
              <div key={g.status}>
                <div className="sticky top-0 z-10 flex items-center gap-2 bg-hull-900/95 backdrop-blur-sm px-5 py-2 border-b border-hull-800/40">
                  <span className={`h-2 w-2 rounded-full ${statusDot[g.status]}`} />
                  <span className="text-[10px] font-semibold uppercase tracking-wider text-gray-500">{statusLabel[g.status]} — {g.items.length}</span>
                </div>
                <div className="divide-y divide-hull-800/50">
                  {g.items.map(u => (
                    <div key={u.email} className="flex items-center gap-3 px-5 py-3 hover:bg-hull-800/30 transition-colors">
                      <div className="relative">
                        <div className="flex h-8 w-8 items-center justify-center rounded-full bg-gradient-to-br from-neon-cyan/20 to-neon-green/10 text-[10px] font-bold text-neon-cyan ring-1 ring-neon-cyan/20">
                          {u.email.split('@')[0].split('.').map(p => p[0]?.toUpperCase()).join('').slice(0, 2)}
                        </div>
                        <span className="absolute -bottom-0.5 -right-0.5 h-2.5 w-2.5 rounded-full bg-neon-green ring-2 ring-hull-900" />
                      </div>
                      <div className="flex-1 min-w-0">
                        <p className={`text-[11px] font-medium truncate ${u.email === currentEmail ? 'text-neon-cyan' : 'text-white'}`}>
                          {u.email.split('@')[0]}{u.email === currentEmail ? ' (you)' : ''}
                        </p>
                        <p className="text-[9px] text-gray-500">{u.email}</p>
                      </div>
                      <div className="text-right shrink-0">
                        <span className={`text-[8px] font-bold uppercase tracking-widest ${u.role === 'admin' ? 'text-neon-cyan' : 'text-gray-600'}`}>{u.role}</span>
                      </div>
                    </div>
                  ))}
                </div>
              </div>
            ))
          )}
        </div>
      </div>
    </div>
  )
}
