import { useCallback, useEffect, useState } from 'react'
import { LogOut, Zap } from 'lucide-react'
import { Badge } from '~/components/ui/badge'
import { Button } from '~/components/ui/button'
import { Skeleton } from '~/components/ui/skeleton'

const API_BASE = import.meta.env['VITE_API_URL'] ?? 'http://localhost:8080'

export function useSchwabAuth() {
  const [authorized, setAuthorized] = useState<boolean | null>(null)
  const [disconnecting, setDisconnecting] = useState(false)

  useEffect(() => {
    const check = () => {
      fetch(`${API_BASE}/api/v1/auth/schwab/status`)
        .then((r) => r.json() as Promise<{ authorized: boolean }>)
        .then((d) => setAuthorized(d.authorized))
        .catch(() => setAuthorized(false))
    }
    check()
    const id = setInterval(check, 30_000)
    return () => clearInterval(id)
  }, [])

  const disconnect = useCallback(() => {
    setDisconnecting(true)
    fetch(`${API_BASE}/api/v1/auth/schwab`, { method: 'DELETE' })
      .then(() => setAuthorized(false))
      .catch(() => {})
      .finally(() => setDisconnecting(false))
  }, [])

  return { authorized, disconnecting, disconnect }
}

interface SchwabAuthBarProps {
  authorized: boolean | null
  disconnecting: boolean
  disconnect: () => void
  connected: boolean
}

export function SchwabAuthBar({ authorized, disconnecting, disconnect, connected }: SchwabAuthBarProps) {
  return (
    <div className="flex items-center justify-between px-1 pb-2 shrink-0 gap-2 flex-wrap">
      <div className="flex items-center gap-2">
        {authorized === null ? (
          <Skeleton className="h-6 w-28" />
        ) : authorized ? (
          <Button
            size="sm"
            variant="outline"
            className="gap-1.5 border-green-700 text-green-400 hover:bg-red-950 hover:text-red-400 hover:border-red-700 h-7 px-2 text-xs"
            onClick={disconnect}
            disabled={disconnecting}
          >
            <LogOut className="h-3 w-3" />
            {disconnecting ? 'Disconnecting…' : 'Schwab Connected'}
          </Button>
        ) : (
          <a href={`${API_BASE}/auth/schwab`}>
            <Button size="sm" variant="outline" className="gap-1.5 h-7 px-2 text-xs">
              <Zap className="h-3 w-3" />
              Connect Schwab
            </Button>
          </a>
        )}
      </div>
      <Badge variant={connected ? 'default' : 'destructive'} className="gap-1.5">
        <span className={`h-1.5 w-1.5 rounded-full ${connected ? 'bg-green-400' : 'bg-red-400'}`} />
        {connected ? 'Live' : 'Disconnected'}
      </Badge>
    </div>
  )
}
