import {
  Archive,
  File as FileIcon,
  FileAudio,
  FileCode,
  FileText,
  FileVideo,
  Folder,
  Image as ImageIcon,
} from 'lucide-react'
import { kindOf } from '../lib/format'

/**
 * Entry icons carry the accent only for folders, so a listing reads as a
 * structure first and a set of files second.
 */
export function EntryIcon({
  name,
  mime,
  isDir,
  size = 18,
}: {
  name: string
  mime?: string
  isDir: boolean
  size?: number
}) {
  if (isDir) {
    return <Folder size={size} className="shrink-0 text-[var(--color-clay)]" fill="currentColor" fillOpacity={0.16} />
  }

  const common = 'shrink-0 text-[var(--faint)]'
  switch (kindOf(name, mime)) {
    case 'image':
      return <ImageIcon size={size} className={common} />
    case 'video':
      return <FileVideo size={size} className={common} />
    case 'audio':
      return <FileAudio size={size} className={common} />
    case 'pdf':
      return <FileText size={size} className={common} />
    case 'text':
      return <FileCode size={size} className={common} />
    case 'archive':
      return <Archive size={size} className={common} />
    default:
      return <FileIcon size={size} className={common} />
  }
}
