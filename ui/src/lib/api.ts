// The API client. Access tokens are short-lived and kept in memory only; the
// refresh token lives in an HttpOnly cookie the script cannot read, so a
// successful XSS cannot walk away with a durable session.

export type Role = 'admin' | 'user'

export interface User {
  id: string
  username: string
  role: Role
  createdAt: string
  updatedAt: string
}

export interface Entry {
  name: string
  path: string
  isDir: boolean
  size: number
  mime?: string
  id: string
  segmentCount?: number
  status?: string
  modifiedAt: number
  createdAt: number
}

export interface Crumb {
  name: string
  path: string
}

export interface Listing {
  path: string
  entries: Entry[]
  breadcrumbs: Crumb[]
}

export interface TelegramStatus {
  state: 'unconfigured' | 'connecting' | 'unauthorized' | 'ready' | 'error'
  error?: string
  userId?: number
  username?: string
  firstName?: string
  phone?: string
  premium: boolean
  dc?: number
  awaitingCode: boolean
  awaitingPassword: boolean
}

export interface TelegramAccountExport {
  format: 'tdrive-telegram-account'
  version: number
  appId: number
  appHash: string
  session: string
}

export interface Status {
  needsSetup: boolean
  telegram: TelegramStatus
  hasChannel: boolean
  version: string
  segmentSize: number
  webdavPath?: string
}

export interface UploadJob {
  id: string
  fileId?: string
  dirId?: string
  name: string
  totalSize: number
  segmentSize: number
  segmentCount: number
  uploadedBytes: number
  status: 'pending' | 'running' | 'complete' | 'failed' | 'cancelled'
  error?: string
  source?: string
  sourceUrl?: string
  createdAt: string
  updatedAt: string
}

export interface SegmentBound {
  index: number
  start: number
  size: number
}

export interface UploadPlan {
  job: UploadJob
  segmentSize: number
  segmentBounds: SegmentBound[]
  pending: number[]
}

export interface Channel {
  id: string
  tgId: number
  title: string
  isDefault: boolean
  createdAt: string
}

export interface ChannelOption {
  tgId: number
  accessHash?: number
  title: string
  username?: string
  canPost: boolean
  participants?: number
}

export interface Stats {
  dirs: number
  files: number
  segments: number
  totalBytes: number
  brokenFiles: number
  pendingFiles: number
}

export interface IndexStatus {
  scanned: number
  dirs: number
  files: number
  segments: number
  broken: number
  running: boolean
  done: boolean
  error?: string
  startedAt?: number
  finishedAt?: number
}

export class ApiError extends Error {
  status: number
  code?: string
  constructor(status: number, message: string, code?: string) {
    super(message)
    this.status = status
    this.code = code
  }
}

let accessToken: string | null = null
let refreshing: Promise<boolean> | null = null

export function setAccessToken(token: string | null) {
  accessToken = token
}

export function hasAccessToken() {
  return accessToken !== null
}

// A single in-flight refresh is shared by every request that discovers an
// expired token at the same moment. Without this, opening the app after a
// long idle fires one refresh per pending request and all but one of them
// fail against the rotated token.
async function refreshOnce(): Promise<boolean> {
  if (!refreshing) {
    refreshing = (async () => {
      try {
        const res = await fetch('/api/auth/refresh', {
          method: 'POST',
          credentials: 'same-origin',
        })
        if (!res.ok) return false
        const body = await res.json()
        accessToken = body.tokens.accessToken
        return true
      } catch {
        return false
      } finally {
        // Cleared on the next tick so callers awaiting this promise all see
        // the same result before a new attempt can start.
        setTimeout(() => {
          refreshing = null
        }, 0)
      }
    })()
  }
  return refreshing
}

interface RequestOptions extends RequestInit {
  /** Skip the automatic retry after a refresh; used by the refresh call. */
  noRetry?: boolean
}

export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { noRetry, ...init } = options
  const headers = new Headers(init.headers)
  if (accessToken) headers.set('Authorization', `Bearer ${accessToken}`)
  if (init.body && !(init.body instanceof Blob) && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }

  const res = await fetch(`/api${path}`, {
    ...init,
    headers,
    credentials: 'same-origin',
  })

  if (res.status === 401 && !noRetry) {
    if (await refreshOnce()) {
      return request<T>(path, { ...options, noRetry: true })
    }
  }

  if (!res.ok) {
    let message = res.statusText
    let code: string | undefined
    try {
      const body = await res.json()
      if (body?.error) message = body.error
      code = body?.code
    } catch {
      /* a non-JSON error body is still an error */
    }
    throw new ApiError(res.status, message, code)
  }

  if (res.status === 204) return undefined as T
  const text = await res.text()
  return (text ? JSON.parse(text) : undefined) as T
}

const json = (body: unknown) => JSON.stringify(body)

export const api = {
  status: () => request<Status>('/status'),
  stats: () => request<Stats>('/stats'),
  me: () => request<User>('/me'),

  setup: (username: string, password: string) =>
    request<{ tokens: { accessToken: string }; user: User }>('/setup', {
      method: 'POST',
      body: json({ username, password }),
    }),

  login: (username: string, password: string) =>
    request<{ tokens: { accessToken: string }; user: User }>('/auth/login', {
      method: 'POST',
      body: json({ username, password }),
    }),

  refresh: refreshOnce,
  logout: () => request<void>('/auth/logout', { method: 'POST' }),

  changeOwnPassword: (current: string, next: string) =>
    request<void>('/me/password', { method: 'POST', body: json({ current, new: next }) }),

  list: (path: string) => request<Listing>(`/fs/list?path=${encodeURIComponent(path)}`),
  stat: (path: string) => request<Entry>(`/fs/stat?path=${encodeURIComponent(path)}`),
  mkdir: (path: string) => request<Entry>('/fs/mkdir', { method: 'POST', body: json({ path }) }),
  rename: (path: string, name: string) =>
    request<Entry>('/fs/rename', { method: 'POST', body: json({ path, name }) }),
  move: (path: string, to: string) =>
    request<Entry>('/fs/move', { method: 'POST', body: json({ path, to }) }),
  remove: (paths: string[]) =>
    request<void>('/fs/delete', { method: 'POST', body: json({ paths }) }),

  segments: (id: string) =>
    request<{ file: Entry; segments: { index: number; size: number; messageId: number }[] }>(
      `/files/${id}/segments`,
    ),

  beginUpload: (body: {
    path: string
    name: string
    size: number
    mime?: string
    overwrite?: boolean
  }) => request<UploadPlan>('/uploads', { method: 'POST', body: json(body) }),

  jobs: () => request<UploadJob[]>('/uploads'),
  job: (id: string) => request<UploadPlan>(`/uploads/${id}`),
  completeUpload: (id: string) => request<Entry>(`/uploads/${id}/complete`, { method: 'POST' }),
  cancelUpload: (id: string) => request<void>(`/uploads/${id}`, { method: 'DELETE' }),

  remoteUpload: (body: { url: string; path: string; name?: string; overwrite?: boolean }) =>
    request<UploadJob>('/uploads/remote', { method: 'POST', body: json(body) }),

  telegramStatus: () => request<TelegramStatus>('/tg/status'),
  configureTelegram: (appId: number, appHash: string) =>
    request<TelegramStatus>('/tg/configure', { method: 'POST', body: json({ appId, appHash }) }),
  sendCode: (phone: string) =>
    request<{ delivery: string; codeLength?: number; alreadyAuthorized: boolean }>(
      '/tg/login/code',
      { method: 'POST', body: json({ phone }) },
    ),
  signIn: (code: string) =>
    request<{ needsPassword: boolean; passwordHint?: string }>('/tg/login/signin', {
      method: 'POST',
      body: json({ code }),
    }),
  submitTelegramPassword: (password: string) =>
    request<TelegramStatus>('/tg/login/password', { method: 'POST', body: json({ password }) }),
  telegramLogout: () => request<TelegramStatus>('/tg/logout', { method: 'POST' }),
  exportTelegramAccount: () => request<TelegramAccountExport>('/tg/account/export'),
  importTelegramAccount: (body: TelegramAccountExport) =>
    request<TelegramStatus>('/tg/account/import', { method: 'POST', body: json(body) }),

  channels: () =>
    request<{ channels: ChannelOption[]; selected: number }>('/tg/channels'),
  createChannel: (title: string) =>
    request<Channel>('/tg/channels', { method: 'POST', body: json({ title }) }),
  selectChannel: (tgId: number, accessHash: number) =>
    request<Channel>('/tg/channels/select', { method: 'POST', body: json({ tgId, accessHash }) }),

  users: () => request<User[]>('/users'),
  createUser: (username: string, password: string, role: Role) =>
    request<User>('/users', { method: 'POST', body: json({ username, password, role }) }),
  deleteUser: (id: string) => request<void>(`/users/${id}`, { method: 'DELETE' }),
  setUserPassword: (id: string, password: string) =>
    request<void>(`/users/${id}/password`, { method: 'POST', body: json({ password }) }),
  setUserRole: (id: string, role: Role) =>
    request<void>(`/users/${id}/role`, { method: 'POST', body: json({ role }) }),

  rebuildIndex: () => request<IndexStatus>('/index/rebuild', { method: 'POST' }),
  indexStatus: () => request<IndexStatus>('/index/status'),
}

/** rawUrl builds a direct link to a file's bytes, for <video> and downloads. */
export function rawUrl(id: string, download = false) {
  return `/api/files/${id}/raw${download ? '?download=1' : ''}`
}

/** currentToken exposes the access token for the few places that need to
 *  authenticate outside the fetch wrapper, such as the EventSource polyfill. */
export function currentToken() {
  return accessToken
}
