import { Code, ConnectError, createClient, type Interceptor } from "@connectrpc/connect"
import { createConnectTransport } from "@connectrpc/connect-web"

import { WorkspaceService } from "@/gen/devopus/workspace/v1/workspace_pb"

// The workspace server serves this app, its API and the sign-in: the refresh token
// stays in an HttpOnly cookie the server sets; the page keeps only a short-lived access
// token in memory.

export type Session = {
  accessToken: string
  expiresAt: string
  email: string
  userId: string
  created?: boolean
}

export type Plan = { id: number; name: string; description: string; available: boolean }

let session: Session | null = null
let refreshing: Promise<Session | null> | null = null
const listeners = new Set<(s: Session | null) => void>()

function set(s: Session | null) {
  session = s
  listeners.forEach((l) => l(s))
}

export function onSession(l: (s: Session | null) => void): () => void {
  listeners.add(l)
  return () => listeners.delete(l)
}

async function post<T>(path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", "X-Devopus": "1" },
    body: JSON.stringify(body ?? {}),
  })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) {
    const err = new Error(data.error || res.statusText) as Error & { status: number }
    err.status = res.status
    throw err
  }
  return data as T
}

// deviceId lets Auth limit codes per browser.
function deviceId(): string {
  try {
    let id = localStorage.getItem("devopus.device")
    if (!id) {
      id = crypto.randomUUID()
      localStorage.setItem("devopus.device", id)
    }
    return id
  } catch {
    return ""
  }
}

export const auth = {
  plans: () => post<Plan[]>("/web/auth/plans"),
  start: (email: string, register: boolean) =>
    post<{ challengeId: string; expiresAt: string }>("/web/auth/start", {
      email,
      register,
      deviceId: deviceId(),
    }),
  async complete(challengeId: string, code: string, plan: number) {
    const s = await post<Session>("/web/auth/complete", { challengeId, code, plan })
    set(s)
    return s
  },
  // resume signs in again from the cookie after a reload; null when signed out.
  resume: () => refresh(),
  async signOut() {
    await post("/web/auth/logout").catch(() => undefined)
    set(null)
  },
}

function refresh(): Promise<Session | null> {
  refreshing ??= post<Session>("/web/auth/refresh")
    .then((s) => {
      const signed = s.accessToken ? s : null
      set(signed)
      return signed
    })
    .catch(() => {
      set(null)
      return null
    })
    .finally(() => {
      refreshing = null
    })
  return refreshing
}

async function accessToken(force = false): Promise<string> {
  const fresh = session && Date.parse(session.expiresAt) - Date.now() > 60_000
  const s = !force && fresh ? session : await refresh()
  if (!s) throw new ConnectError("signed out", Code.Unauthenticated)
  return s.accessToken
}

// bearer signs every call; a refused token is refreshed once and the call repeated.
const bearer: Interceptor = (next) => async (req) => {
  req.header.set("Authorization", `Bearer ${await accessToken()}`)
  try {
    return await next(req)
  } catch (err) {
    if (ConnectError.from(err).code !== Code.Unauthenticated) throw err
    req.header.set("Authorization", `Bearer ${await accessToken(true)}`)
    return next(req)
  }
}

const transport = createConnectTransport({
  baseUrl: window.location.origin,
  interceptors: [bearer],
})

export const ws = createClient(WorkspaceService, transport)

export function errorText(err: unknown): string {
  if (err instanceof Error && !(err instanceof ConnectError) && "status" in err) {
    return err.message
  }
  return ConnectError.from(err).rawMessage
}
