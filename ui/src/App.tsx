import { useEffect } from 'react'
import { AppProvider, useApp, useRoute } from './app/context'
import { Shell } from './components/Shell'
import { Spinner, ToastHost } from './components/primitives'
import { Files } from './routes/Files'
import { Login } from './routes/Login'
import { PluginList, PluginView } from './routes/Plugins'
import { Setup } from './routes/Setup'
import { Settings } from './routes/settings'
import { Transfers } from './routes/Transfers'

export default function App() {
  return (
    <AppProvider>
      <Router />
      <ToastHost />
    </AppProvider>
  )
}

function Router() {
  const { ready, status, user } = useApp()
  const { path, navigate } = useRoute()

  // Creating the first WebUI account is the only mandatory first-run step.
  // Telegram credentials, login and the storage channel can be configured from
  // Settings later, so a fresh deployment is usable even before Telegram is
  // connected.
  const needsWizard = status?.needsSetup

  useEffect(() => {
    if (!ready) return
    if (path === '/') navigate(user ? '/files' : '/login', true)
    else if (user && path === '/login') navigate('/files', true)
  }, [ready, path, user, navigate])

  if (!ready) {
    return (
      <div className="flex h-full items-center justify-center">
        <Spinner className="size-6" />
      </div>
    )
  }

  if (!user && !status?.needsSetup) {
    return <Login />
  }
  // A successful login updates the session but the login form does not own
  // the route. Keep the stale URL from being interpreted as a drive path
  // while the redirect effect above replaces it with the files route.
  if (user && path === '/login') {
    return (
      <div className="flex h-full items-center justify-center">
        <Spinner className="size-6" />
      </div>
    )
  }
  if (needsWizard) {
    return <Setup />
  }

  if (path.startsWith('/transfers')) {
    return (
      <Shell active="transfers" path="/" onNavigate={navigate}>
        <Transfers />
      </Shell>
    )
  }
  if (path.startsWith('/settings')) {
    return (
      <Shell active="settings" path="/" onNavigate={navigate}>
        <Settings onNavigate={navigate} />
      </Shell>
    )
  }

  // Plugin routes have to be matched before the drive fallthrough below, which
  // reads any unmatched path as a folder name. The '/plugin/' prefix is spelled
  // out rather than using startsWith('/plugin') so a future /plugin-something
  // cannot be swallowed by it.
  if (path === '/plugin') {
    return (
      <Shell active="plugins" path="/" onNavigate={navigate}>
        <PluginList onNavigate={navigate} />
      </Shell>
    )
  }
  if (path.startsWith('/plugin/')) {
    const id = decodeURIComponent(path.slice('/plugin/'.length))
    return (
      <Shell active={`plugin:${id}`} path="/" onNavigate={navigate}>
        <PluginView id={id} onNavigate={navigate} />
      </Shell>
    )
  }

  // Everything else is a drive path: /files/电影/2024 browses /电影/2024.
  const drivePath = decodeURI(path.replace(/^\/files/, '')) || '/'
  return (
    <Shell active="files" path={drivePath} onNavigate={navigate}>
      <Files path={drivePath} onNavigate={navigate} />
    </Shell>
  )
}
