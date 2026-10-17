import * as React from "react"
import { LogOutIcon, MonitorIcon, MoonIcon, PlusIcon, SunIcon } from "lucide-react"

import { useTheme } from "@/components/theme-provider"
import { Button, buttonVariants } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"
import { Toaster } from "@/components/ui/toast"
import { TooltipProvider } from "@/components/ui/tooltip"
import type { GetWorkspaceResponse, Workspace } from "@/gen/devopus/workspace/v1/workspace_pb"
import { cn } from "@/lib/utils"
import { auth, onSession, ws, type Session } from "@ws/api"
import { AcceptInvite, CreateWorkspace } from "@ws/dialogs"
import { Audit } from "@ws/pages/audit"
import { Invites } from "@ws/pages/invites"
import { Members } from "@ws/pages/members"
import { Overview } from "@ws/pages/overview"
import { Projects } from "@ws/pages/projects"
import { SignIn } from "@ws/sign-in"

// The current workspace and what the signed-in user may do in it, for every page.
export type Current = {
  workspace: Workspace
  info: GetWorkspaceResponse
  can: (permission: string, project?: string) => boolean
  reload: () => void
}

const pages = [
  { id: "overview", title: "Overview" },
  { id: "members", title: "Members" },
  { id: "invites", title: "Invites", needs: "members.invite" },
  { id: "projects", title: "Projects" },
  { id: "audit", title: "Audit log", needs: "workspace.manage" },
] as const

type PageId = (typeof pages)[number]["id"]

// The page lives in the URL hash: #/members.
function usePage(): [PageId, (p: PageId) => void] {
  const read = (): PageId => {
    const id = window.location.hash.replace(/^#\/?/, "")
    return (pages.find((p) => p.id === id)?.id ?? "overview") as PageId
  }
  const [page, setPage] = React.useState(read)
  React.useEffect(() => {
    const on = () => setPage(read())
    window.addEventListener("hashchange", on)
    return () => window.removeEventListener("hashchange", on)
  }, [])
  return [page, (p) => (window.location.hash = `/${p}`)]
}

export function App() {
  const [session, setSession] = React.useState<Session | null | undefined>(undefined)
  React.useEffect(() => {
    const off = onSession(setSession)
    auth.resume()
    return () => {
      off()
    }
  }, [])
  if (session === undefined) return <Loading />
  if (session === null) return <SignIn />
  return (
    <TooltipProvider>
      <Signed session={session} />
      <Toaster />
    </TooltipProvider>
  )
}

function Loading() {
  return (
    <div className="grid min-h-svh place-items-center">
      <Skeleton className="h-6 w-40" />
    </div>
  )
}

const chosenKey = "devopus.workspace"

function Signed({ session }: { session: Session }) {
  const [list, setList] = React.useState<Workspace[]>()
  const [chosen, setChosen] = React.useState<string | null>(() => {
    try {
      return localStorage.getItem(chosenKey)
    } catch {
      return null
    }
  })
  const [info, setInfo] = React.useState<GetWorkspaceResponse>()
  const [creating, setCreating] = React.useState(false)
  const [accepting, setAccepting] = React.useState(false)
  const [page, setPage] = usePage()
  const [tick, setTick] = React.useState(0)

  const loadList = React.useCallback(() => {
    ws.listWorkspaces({}).then((r) => setList(r.workspaces))
  }, [])
  React.useEffect(loadList, [loadList])

  const workspace = list?.find((w) => w.id === chosen) ?? list?.[0]
  React.useEffect(() => {
    if (!workspace) return
    let stale = false
    ws.getWorkspace({ workspaceId: workspace.id }).then((r) => !stale && setInfo(r))
    return () => {
      stale = true
    }
  }, [workspace, tick])

  const choose = (id: string) => {
    setChosen(id)
    setInfo(undefined)
    try {
      localStorage.setItem(chosenKey, id)
    } catch {
      /* the choice is a convenience */
    }
  }

  const current: Current | undefined =
    workspace && info && info.workspace?.id === workspace.id
      ? {
          workspace,
          info,
          can: (perm, project) =>
            project
              ? info.workspace?.role === "admin" ||
                !!info.projects.find((p) => p.project === project)?.permissions.includes(perm)
              : info.permissions.includes(perm),
          reload: () => {
            setTick((t) => t + 1)
            loadList()
          },
        }
      : undefined

  const visible = pages.filter((p) => !("needs" in p) || current?.can(p.needs))

  return (
    <div className="grid min-h-svh md:grid-cols-[16rem_1fr]">
      <aside className="flex flex-col gap-6 border-b bg-sidebar px-3 py-5 md:sticky md:top-0 md:h-svh md:border-r md:border-b-0">
        <div className="flex items-center justify-between px-3">
          <span className="text-lg font-semibold tracking-tight">Devopus</span>
          <ThemeToggle />
        </div>
        {list && list.length > 0 && workspace && (
          <DropdownMenu>
            <DropdownMenuTrigger className={cn(buttonVariants({ variant: "outline" }), "justify-between")}>
              <span className="truncate">{workspace.name}</span>
              <span className="text-xs text-muted-foreground">{workspace.role}</span>
            </DropdownMenuTrigger>
            <DropdownMenuContent className="min-w-56">
              {list.map((w) => (
                <DropdownMenuItem key={w.id} onClick={() => choose(w.id)}>
                  <span className="flex-1 truncate">{w.name}</span>
                  <span className="text-xs text-muted-foreground">{w.role}</span>
                </DropdownMenuItem>
              ))}
              <DropdownMenuSeparator />
              <DropdownMenuItem onClick={() => setCreating(true)}>
                <PlusIcon /> New workspace
              </DropdownMenuItem>
              <DropdownMenuItem onClick={() => setAccepting(true)}>Accept an invite</DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        )}
        <nav className="flex flex-col gap-1">
          {current &&
            visible.map((p) => (
              <button
                key={p.id}
                onClick={() => setPage(p.id)}
                className={cn(
                  "rounded-md px-3 py-1.5 text-left text-sm hover:bg-sidebar-accent",
                  page === p.id && "bg-sidebar-accent font-medium"
                )}
              >
                {p.title}
              </button>
            ))}
        </nav>
        <div className="mt-auto flex items-center justify-between gap-2 px-3 text-sm">
          <span className="truncate text-sidebar-foreground/70">{session.email}</span>
          <Button variant="ghost" size="icon-sm" aria-label="Sign out" onClick={() => auth.signOut()}>
            <LogOutIcon />
          </Button>
        </div>
      </aside>
      <main className="min-w-0 px-4 py-6 md:px-10 md:py-8">
        {list === undefined ? (
          <Skeleton className="h-8 w-64" />
        ) : list.length === 0 ? (
          <Empty>
            <EmptyHeader>
              <EmptyTitle>No workspaces yet</EmptyTitle>
              <EmptyDescription>
                Create one for your team — you become its admin — or accept an invite you got by email.
              </EmptyDescription>
            </EmptyHeader>
            <EmptyContent className="flex-row justify-center">
              <Button onClick={() => setCreating(true)}>Create a workspace</Button>
              <Button variant="outline" onClick={() => setAccepting(true)}>
                Accept an invite
              </Button>
            </EmptyContent>
          </Empty>
        ) : !current ? (
          <Skeleton className="h-8 w-64" />
        ) : page === "members" ? (
          <Members current={current} />
        ) : page === "invites" && current.can("members.invite") ? (
          <Invites current={current} />
        ) : page === "projects" ? (
          <Projects current={current} />
        ) : page === "audit" && current.can("workspace.manage") ? (
          <Audit current={current} />
        ) : (
          <Overview current={current} onDeleted={() => (setChosen(null), loadList())} />
        )}
      </main>
      <CreateWorkspace
        open={creating}
        onClose={() => setCreating(false)}
        onCreated={(id) => (choose(id), loadList(), setPage("overview"))}
      />
      <AcceptInvite
        open={accepting}
        onClose={() => setAccepting(false)}
        onAccepted={(id) => (choose(id), loadList(), setPage("overview"))}
      />
    </div>
  )
}

function ThemeToggle() {
  const { theme, setTheme } = useTheme()
  const next = theme === "light" ? "dark" : theme === "dark" ? "system" : "light"
  const Icon = theme === "light" ? SunIcon : theme === "dark" ? MoonIcon : MonitorIcon
  return (
    <Button variant="ghost" size="icon-sm" aria-label={`Theme: ${theme}`} onClick={() => setTheme(next)}>
      <Icon />
    </Button>
  )
}
