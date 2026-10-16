import { Code, ConnectError, createClient } from "@connectrpc/connect"
import { createConnectTransport } from "@connectrpc/connect-web"

import { AgentService } from "@/gen/devopus/agent/v1/agent_pb"

// The agent serves the panel and its API from the same origin; the session cookie set
// by /auth goes along with every request.
const transport = createConnectTransport({ baseUrl: window.location.origin })

export const agent = createClient(AgentService, transport)

export function isUnauthenticated(err: unknown): boolean {
  return ConnectError.from(err).code === Code.Unauthenticated
}

export function errorText(err: unknown): string {
  return ConnectError.from(err).rawMessage
}
