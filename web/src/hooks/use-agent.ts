import * as React from "react"

import type {
  EnvironmentStatus,
  StatusResponse,
} from "@/gen/devopus/agent/v2/agent_pb"
import { agent, isUnauthenticated } from "@/lib/api"

export type Connection = "connecting" | "live" | "lost" | "signed-out"

export interface AgentState {
  connection: Connection
  status?: StatusResponse
  envs: EnvironmentStatus[]
  refresh: () => void
}

const STATUS_EVERY = 5000
const RETRY_AFTER = 2000

// useAgent follows the agent: environments live through Watch, the rest (projects,
// proxy, runtime) from Status every few seconds.
export function useAgent(): AgentState {
  const [connection, setConnection] = React.useState<Connection>("connecting")
  const [status, setStatus] = React.useState<StatusResponse>()
  const [envs, setEnvs] = React.useState<Map<string, EnvironmentStatus>>(
    () => new Map()
  )
  const [tick, setTick] = React.useState(0)

  React.useEffect(() => {
    let stopped = false
    let timer: number | undefined
    const load = async () => {
      try {
        const s = await agent.status({})
        if (!stopped) setStatus(s)
      } catch (err) {
        if (!stopped && isUnauthenticated(err)) setConnection("signed-out")
      }
      if (!stopped) timer = window.setTimeout(load, STATUS_EVERY)
    }
    load()
    return () => {
      stopped = true
      window.clearTimeout(timer)
    }
  }, [tick])

  React.useEffect(() => {
    const abort = new AbortController()
    const run = async () => {
      while (!abort.signal.aborted) {
        // The current statuses come first; until synced they replace what we had,
        // so environments removed while we were away disappear.
        let fresh: Map<string, EnvironmentStatus> | null = new Map()
        try {
          for await (const res of agent.watch({}, { signal: abort.signal })) {
            if (res.synced && fresh) {
              const snapshot = fresh
              fresh = null
              setEnvs(snapshot)
              setConnection("live")
              continue
            }
            const st = res.status
            if (!st) continue
            if (fresh) {
              if (!st.removed) fresh.set(st.id, st)
              continue
            }
            setEnvs((prev) => {
              const next = new Map(prev)
              if (st.removed) next.delete(st.id)
              else next.set(st.id, st)
              return next
            })
          }
        } catch (err) {
          if (abort.signal.aborted) return
          if (isUnauthenticated(err)) {
            setConnection("signed-out")
            return
          }
        }
        if (abort.signal.aborted) return
        setConnection("lost")
        await new Promise((r) => setTimeout(r, RETRY_AFTER))
      }
    }
    run()
    return () => abort.abort()
  }, [tick])

  const list = React.useMemo(() => [...envs.values()], [envs])
  const refresh = React.useCallback(() => setTick((t) => t + 1), [])
  return { connection, status, envs: list, refresh }
}
