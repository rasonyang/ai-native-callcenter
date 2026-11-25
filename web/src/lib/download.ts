/** Hands the operator a file the browser holds, under the given name. */
export function downloadBlob(name: string, blob: Blob): void {
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = name
  link.click()
  URL.revokeObjectURL(url)
}

/** Hands the operator a CSV file made in the browser. */
export function downloadCsv(name: string, text: string): void {
  downloadBlob(name, new Blob([text], { type: 'text/csv;charset=utf-8' }))
}
