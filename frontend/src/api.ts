export class ApiError extends Error {
  status: number
  constructor(message: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

async function request<T>(url: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers)
  headers.set('X-Tami-Client', 'desktop')
  if (typeof init?.body === 'string' && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json')
  const response = await fetch(url, { ...init, headers })
  if (!response.ok) {
    let message = `请求失败（${response.status}）`
    try {
      const body = await response.json() as { error?: string }
      if (body.error) message = body.error
    } catch { /* Keep the status-based message for non-JSON responses. */ }
    throw new ApiError(message, response.status)
  }
  if (response.status === 204) return undefined as T
  const text = await response.text()
  return (text ? JSON.parse(text) : undefined) as T
}

export const api = {
  get<T>(url: string) { return request<T>(url) },
  post<T>(url: string, body?: unknown) {
    const isFile = typeof File !== 'undefined' && body instanceof File
    return request<T>(url, {
      method: 'POST',
      headers: isFile ? undefined : body === undefined ? undefined : { 'Content-Type': 'application/json' },
      body: isFile ? body : body === undefined ? undefined : JSON.stringify(body),
    })
  },
  upload<T>(url: string, file: File) { return request<T>(url, { method: 'POST', body: file }) },
}
