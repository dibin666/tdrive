import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { api, setAccessToken, type Status, type User } from '../lib/api'
import { events } from '../lib/events'

type Theme = 'light' | 'dark'

interface AppState {
  ready: boolean
  status: Status | null
  user: User | null
  theme: Theme
  toggleTheme: () => void
  refreshStatus: () => Promise<void>
  signIn: (username: string, password: string) => Promise<void>
  completeSetup: (username: string, password: string) => Promise<void>
  signOut: () => Promise<void>
}

const AppContext = createContext<AppState | null>(null)

export function useApp() {
  const ctx = useContext(AppContext)
  if (!ctx) throw new Error('useApp must be used inside AppProvider')
  return ctx
}

export function AppProvider({ children }: { children: ReactNode }) {
  const [ready, setReady] = useState(false)
  const [status, setStatus] = useState<Status | null>(null)
  const [user, setUser] = useState<User | null>(null)
  const [theme, setTheme] = useState<Theme>(() =>
    document.documentElement.classList.contains('dark') ? 'dark' : 'light',
  )

  const refreshStatus = useCallback(async () => {
    try {
      setStatus(await api.status())
    } catch {
      /* the status endpoint is open, so a failure means the server is down */
    }
  }, [])

  // Boot: try to resume a session from the refresh cookie before deciding
  // whether to show the login form, so a reload does not bounce the user out.
  useEffect(() => {
    let cancelled = false
    ;(async () => {
      await refreshStatus()
      if (await api.refresh()) {
        try {
          const me = await api.me()
          if (!cancelled) setUser(me)
        } catch {
          setAccessToken(null)
        }
      }
      if (!cancelled) setReady(true)
    })()
    return () => {
      cancelled = true
    }
  }, [refreshStatus])

  // Telegram state changes arrive over the event stream, so the setup wizard
  // advances on its own instead of asking the user to refresh.
  useEffect(() => {
    if (!user) return
    return events.subscribe((event) => {
      if (event.type === 'telegram') {
        setStatus((prev) => (prev ? { ...prev, telegram: event.data as Status['telegram'] } : prev))
      }
    })
  }, [user])

  // The access token expires every fifteen minutes; refreshing on a shorter
  // interval keeps long-running uploads from tripping over an expiry.
  const refreshTimer = useRef<number | undefined>(undefined)
  useEffect(() => {
    if (!user) return
    refreshTimer.current = window.setInterval(() => {
      void api.refresh()
    }, 10 * 60 * 1000)
    return () => window.clearInterval(refreshTimer.current)
  }, [user])

  const toggleTheme = useCallback(() => {
    setTheme((prev) => {
      const next = prev === 'dark' ? 'light' : 'dark'
      document.documentElement.classList.toggle('dark', next === 'dark')
      try {
        localStorage.setItem('tdrive.theme', next)
      } catch {
        /* private browsing */
      }
      return next
    })
  }, [])

  const signIn = useCallback(
    async (username: string, password: string) => {
      const res = await api.login(username, password)
      setAccessToken(res.tokens.accessToken)
      setUser(res.user)
      await refreshStatus()
    },
    [refreshStatus],
  )

  const completeSetup = useCallback(
    async (username: string, password: string) => {
      const res = await api.setup(username, password)
      setAccessToken(res.tokens.accessToken)
      setUser(res.user)
      await refreshStatus()
    },
    [refreshStatus],
  )

  const signOut = useCallback(async () => {
    try {
      await api.logout()
    } finally {
      setAccessToken(null)
      setUser(null)
      events.stop()
      await refreshStatus()
    }
  }, [refreshStatus])

  const value = useMemo<AppState>(
    () => ({ ready, status, user, theme, toggleTheme, refreshStatus, signIn, completeSetup, signOut }),
    [ready, status, user, theme, toggleTheme, refreshStatus, signIn, completeSetup, signOut],
  )

  return <AppContext.Provider value={value}>{children}</AppContext.Provider>
}

/**
 * useRoute is a minimal history-based router.
 *
 * The app has four destinations and one of them carries a path. A routing
 * library would be more code than this and one more thing to keep current.
 */
export function useRoute() {
  const [path, setPath] = useState(() => window.location.pathname)

  useEffect(() => {
    const onPop = () => setPath(window.location.pathname)
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])

  const navigate = useCallback((to: string, replace = false) => {
    if (replace) window.history.replaceState(null, '', to)
    else window.history.pushState(null, '', to)
    setPath(to)
  }, [])

  return { path, navigate }
}
