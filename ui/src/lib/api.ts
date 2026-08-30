// The API client. Access tokens are short-lived and kept in memory only; the
// refresh token lives in an HttpOnly cookie the script cannot read, so a
// successful XSS cannot walk away with a durable session.

export type Role = 'admin' | 'user'

/** Every permission the server knows. The catalogue endpoint returns the
 *  authoritative list; this type exists so the UI cannot typo one. */
export type Perm =
  | 'read'
  | 'download'
  | 'upload'
  | 'uploadLocal'
  | 'remoteFetch'
  | 'mkdir'
  | 'rename'
  | 'move'
  | 'delete'
  | 'webdav'
  | 'stage'
  | 'share'

export interface User {
  id: string
  username: string
  role: Role
  createdAt: string
  updatedAt: string
  enabled: boolean
  scopePath: string
  quotaBytes: number
  note: string
  lastLoginIp?: string
  lastLoginAt?: number
  perms: Perm[]
  /** True when the permissions come from the role rather than a stored mask. */
  permsInherited: boolean
  usedBytes: number
  fileCount: number
  sessions: number
}

export interface Session {
  id: string
  userAgent: string
  ip: string
  createdAt: string
  lastUsedAt: string
  expiresAt: string
  current: boolean
}

export interface AuditEntry {
  id: string
  at: string
  actorId?: string
  actorName: string
  action: string
  target?: string
  detail?: string
  ip?: string
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
  localEnabled: boolean
  webdavPath?: string
}

export interface LocalEntry {
  name: string
  path: string
  isDir: boolean
  size: number
  modifiedAt: number
}

export interface LocalListing {
  path: string
  entries: LocalEntry[]
  breadcrumbs: Crumb[]
}

export interface RuntimeSettings {
  appId: number
  appHash: string
  localRoot: string
  segmentSize: number
  poolSize: number
  uploadThreads: number
  uploadPartSize: number
  rateLimitMs: number
  streamConcurrency: number
  uploadConcurrency: number
  downloadConcurrency: number
  webdavEnabled: boolean
  logLevel: string
  cacheDir: string
  cacheLimit: number
  cacheTtlHours: number
  maxDownloadConns: number
  downloadGraceMs: number
  shareTtlHours: number
}

export type JobStatus = 'pending' | 'running' | 'complete' | 'failed' | 'cancelled'

export interface UploadJob {
  id: string
  kind?: 'upload'
  fileId?: string
  dirId?: string
  name: string
  totalSize: number
  segmentSize: number
  segmentCount: number
  uploadedBytes: number
  status: JobStatus
  error?: string
  source?: string
  sourceUrl?: string
  createdAt: number | string
  updatedAt: number | string
  startedAt?: number
  finishedAt?: number
  /** Bytes per second across the window the transfer was actually moving. */
  avgSpeed?: number
}

export type DownloadMode = 'direct' | 'staged' | 'segments'
export type DownloadStatus =
  | 'pending'
  | 'running'
  | 'ready'
  | 'complete'
  | 'failed'
  | 'cancelled'
  | 'expired'

export interface DownloadJob {
  id: string
  kind?: 'download'
  fileId?: string
  name: string
  totalSize: number
  downloadedBytes: number
  mode: DownloadMode
  status: DownloadStatus
  error?: string
  createdAt: number
  updatedAt: number
  startedAt?: number
  finishedAt?: number
  expiresAt?: number
  avgSpeed?: number
  url?: string
}

/** One row of the merged transfer list. Exactly one side is populated. */
export interface TransferRow {
  id: string
  kind: 'upload' | 'download'
  name: string
  status: string
  createdAt: number
  upload?: UploadJob
  download?: DownloadJob
}

export interface TransferFilter {
  kind?: 'upload' | 'download'
  status?: string[]
  source?: string[]
  from?: number
  to?: number
  q?: string
  limit?: number
  all?: boolean
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

export interface DownloadModeInfo {
  mode: DownloadMode
  available: boolean
  recommended: boolean
  reason?: string
}

export interface CacheStatus {
  dir: string
  used: number
  limit: number
  files: number
}

export interface DownloadOptions {
  fileId: string
  name: string
  size: number
  mime?: string
  segmentCount: number
  segmentBounds?: SegmentBound[]
  modes: DownloadModeInfo[]
  maxConnections: number
  staged?: DownloadJob
  cache: CacheStatus
}

export interface MediaLink {
  url: string
  download: string
}

export interface ShareLinkBody {
  id: string
  url: string
  kind: 'file' | 'segment'
  index?: number
  name: string
  size: number
  expiresAt?: number
}

export interface ShareResponse {
  file: ShareLinkBody
  segments?: ShareLinkBody[]
}

export interface ShareRecord {
  id: string
  fileId: string
  kind: 'file' | 'segment'
  label?: string
  revoked: boolean
  hits: number
  createdAt: string
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

export interface BatchRenameResult {
  path: string
  name: string
  ok: boolean
  newPath?: string
  error?: string
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

/** query builds a search string, skipping empty values so the URLs stay
 *  readable and the server sees "absent" rather than "empty". */
function query(params: Record<string, string | number | boolean | string[] | undefined>) {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === '' || value === false) continue
    if (Array.isArray(value)) {
      if (value.length === 0) continue
      search.set(key, value.join(','))
    } else {
      search.set(key, String(value))
    }
  }
  const s = search.toString()
  return s ? `?${s}` : ''
}

export const api = {
  status: () => request<Status>('/status'),
  settings: () => request<RuntimeSettings>('/settings'),
  updateSettings: (body: Partial<RuntimeSettings>) =>
    request<RuntimeSettings>('/settings', { method: 'PUT', body: json(body) }),
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
  mySessions: () => request<Session[]>('/me/sessions'),
  revokeMySession: (id: string) => request<void>(`/me/sessions/${id}`, { method: 'DELETE' }),

  list: (path: string) => request<Listing>(`/fs/list?path=${encodeURIComponent(path)}`),
  localList: (path: string) => request<LocalListing>(`/local/list?path=${encodeURIComponent(path)}`),
  stat: (path: string) => request<Entry>(`/fs/stat?path=${encodeURIComponent(path)}`),
  mkdir: (path: string) => request<Entry>('/fs/mkdir', { method: 'POST', body: json({ path }) }),
  rename: (path: string, name: string) =>
    request<Entry>('/fs/rename', { method: 'POST', body: json({ path, name }) }),
  batchRename: (items: { path: string; name: string }[]) =>
    request<{ results: BatchRenameResult[]; renamed: number; failed: number }>('/fs/batch-rename', {
      method: 'POST',
      body: json({ items }),
    }),
  move: (path: string, to: string) =>
    request<Entry>('/fs/move', { method: 'POST', body: json({ path, to }) }),
  remove: (paths: string[]) =>
    request<void>('/fs/delete', { method: 'POST', body: json({ paths }) }),

  segments: (id: string) =>
    request<{ file: Entry; segments: { index: number; size: number; messageId: number }[] }>(
      `/files/${id}/segments`,
    ),

  downloadOptions: (id: string) => request<DownloadOptions>(`/files/${id}/download-options`),
  mediaLink: (id: string) => request<MediaLink>(`/files/${id}/link`),
  share: (id: string, body: { ttlSeconds?: number; segments?: boolean; label?: string } = {}) =>
    request<ShareResponse>(`/files/${id}/share`, { method: 'POST', body: json(body) }),
  shares: (all = false) => request<ShareRecord[]>(`/shares${query({ all })}`),
  revokeShare: (id: string) => request<void>(`/shares/${id}`, { method: 'DELETE' }),

  startDownload: (fileId: string, mode: DownloadMode) =>
    request<DownloadJob>('/downloads', { method: 'POST', body: json({ fileId, mode }) }),
  download: (id: string) => request<DownloadJob>(`/downloads/${id}`),
  reportDownload: (id: string, body: { downloaded?: number; status?: string; error?: string }) =>
    request<DownloadJob>(`/downloads/${id}/progress`, { method: 'POST', body: json(body) }),
  cancelDownload: (id: string) => request<void>(`/downloads/${id}`, { method: 'DELETE' }),

  transfers: (filter: TransferFilter = {}) =>
    request<{ transfers: TransferRow[]; total: number }>(
      `/transfers${query({
        kind: filter.kind,
        status: filter.status,
        source: filter.source,
        from: filter.from,
        to: filter.to,
        q: filter.q,
        limit: filter.limit,
        all: filter.all,
      })}`,
    ),
  deleteTransfers: (body: {
    ids?: string[]
    kind?: 'upload' | 'download'
    statuses?: string[]
    before?: number
    all?: boolean
  }) => request<{ removed: number }>('/transfers', { method: 'DELETE', body: json(body) }),
  deleteTransfer: (kind: 'upload' | 'download', id: string) =>
    request<void>(`/transfers/${kind}/${id}`, { method: 'DELETE' }),

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

  localUpload: (body: { sourcePath: string; path: string; name?: string; overwrite?: boolean }) =>
    request<UploadJob>('/uploads/local', { method: 'POST', body: json(body) }),

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
  permissionCatalog: () =>
    request<{ all: Perm[]; userDefault: Perm[]; adminNote: string }>('/users/permissions'),
  createUser: (body: {
    username: string
    password: string
    role: Role
    perms?: Perm[]
    scopePath?: string
    quotaBytes?: number
    note?: string
  }) => request<User>('/users', { method: 'POST', body: json(body) }),
  updateUser: (
    id: string,
    body: {
      enabled?: boolean
      perms?: Perm[]
      scopePath?: string
      quotaBytes?: number
      note?: string
    },
  ) => request<User>(`/users/${id}`, { method: 'PATCH', body: json(body) }),
  deleteUser: (id: string) => request<void>(`/users/${id}`, { method: 'DELETE' }),
  setUserPassword: (id: string, password: string) =>
    request<void>(`/users/${id}/password`, { method: 'POST', body: json({ password }) }),
  setUserRole: (id: string, role: Role) =>
    request<void>(`/users/${id}/role`, { method: 'POST', body: json({ role }) }),
  userSessions: (id: string) => request<Session[]>(`/users/${id}/sessions`),
  revokeUserSession: (id: string, sid: string) =>
    request<void>(`/users/${id}/sessions/${sid}`, { method: 'DELETE' }),
  revokeUserSessions: (id: string) =>
    request<void>(`/users/${id}/sessions/revoke-all`, { method: 'POST' }),

  audit: (params: { actor?: string; action?: string; from?: number; to?: number; q?: string; limit?: number } = {}) =>
    request<AuditEntry[]>(`/audit${query(params)}`),

  cache: () => request<CacheStatus>('/cache'),
  purgeCache: () => request<{ freed: number }>('/cache/purge', { method: 'POST' }),

  rebuildIndex: () => request<IndexStatus>('/index/rebuild', { method: 'POST' }),
  indexStatus: () => request<IndexStatus>('/index/status'),
}

/** rawUrl builds a direct link to a file's bytes, for <video> and downloads. */
export function rawUrl(id: string, download = false) {
  return `/api/files/${id}/raw${download ? '?download=1' : ''}`
}

/** segmentRawUrl is one stored segment as its own object, which is what the
 *  split-download mode fetches. */
export function segmentRawUrl(id: string, index: number, token?: string) {
  const suffix = token ? `?download=1&t=${encodeURIComponent(token)}` : '?download=1'
  return `/api/files/${id}/segments/${index}/raw${suffix}`
}

/** currentToken exposes the access token for the few places that need to
 *  authenticate outside the fetch wrapper, such as the EventSource polyfill. */
export function currentToken() {
  return accessToken
}

/** can reports whether an account holds a permission. Admins hold everything,
 *  which the server also enforces; this only decides what to render. */
export function can(user: User | null, perm: Perm): boolean {
  if (!user) return false
  if (user.role === 'admin') return true
  return user.perms.includes(perm)
}
