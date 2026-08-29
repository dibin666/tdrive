import { useEffect } from 'react'
import { AppProvider, useApp, useRoute } from './app/context'
import { Shell } from './components/Shell'
import { Spinner, ToastHost } from './components/primitives'
import { Files } from './routes/Files'
import { Login } from './routes/Login'
import { Setup } from './routes/Setup'
import { Settings } from './routes/Settings'
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

  // The drive is unusable until Telegram is connected and a channel is
  // chosen, so an incomplete install is routed to the wizard rather than to a
  // file list that could only ever be empty.
  const needsWizard = status?.needsSetup || status?.telegram.state !== 'ready' || !status?.hasChannel

  useEffect(() => {
    if (!ready) return
    if (path === '/') navigate(user ? '/files' : '/login', true)
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
        <Settings />
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
