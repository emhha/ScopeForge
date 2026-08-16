// HTTP 基础层:token 存取 + JSON 请求封装。
const TOKEN_KEY = 'scopeforge_token'

export function authToken(): string {
  try {
    return localStorage.getItem(TOKEN_KEY) || ''
  } catch {
    return ''
  }
}

export function setAuthToken(token: string): void {
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token)
    else localStorage.removeItem(TOKEN_KEY)
  } catch {
    // localStorage 不可用时静默降级
  }
}

export async function req<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers: Record<string, string> = {
    ...(init.headers as Record<string, string> | undefined),
  }
  if (init.body && !headers['Content-Type']) headers['Content-Type'] = 'application/json'
  const token = authToken()
  if (token) headers['Authorization'] = `Bearer ${token}`

  const resp = await fetch(path, { ...init, headers })
  const text = await resp.text()
  let body: any = undefined
  if (text) {
    try {
      body = JSON.parse(text)
    } catch {
      body = text
    }
  }
  if (!resp.ok) {
    const msg = typeof body === 'string' ? body : body?.error || JSON.stringify(body)
    throw new Error(`${resp.status}: ${String(msg).slice(0, 300)}`)
  }
  return body as T
}
