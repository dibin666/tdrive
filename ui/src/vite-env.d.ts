/// <reference types="vite/client" />

// The File System Access API is what makes a parallel download workable: it is
// the only way to write out-of-order bytes to a real file instead of holding
// the whole thing in memory. TypeScript's DOM library still does not declare
// showSaveFilePicker, so the shape this app uses is declared here.
interface SaveFilePickerOptions {
  suggestedName?: string
  types?: { description?: string; accept: Record<string, string[]> }[]
  excludeAcceptAllOption?: boolean
}

interface Window {
  showSaveFilePicker?(options?: SaveFilePickerOptions): Promise<FileSystemFileHandle>
}
