export type ConnectionKind = 'webdav' | 's3' | 'demo'
export type Direction = 'both' | 'upload' | 'download' | 'mirror-upload' | 'mirror-download'

export interface Connection {
  id: string
  name: string
  kind: ConnectionKind
  endpoint: string
  region: string
  bucket: string
  prefix: string
  username: string
  pathStyle: boolean
  tested: boolean
  capabilities: { conditionalWrite: boolean; conditionalDelete: boolean; rangeRead?: boolean; multipartConditional?: boolean }
  error: string
}
export interface Job {
  id: string
  name: string
  connectionId: string
  localPath: string
  remotePath: string
  direction: Direction
  status: string
  lastRun: string
  lastScanAt?: string
  lastScanSummary?: string
  detail: string
  enabled: boolean
  exclude?: string[]
  scheduleMinutes?: number
  watch?: boolean
  deleteThreshold?: number
  progress?: number
  queueTotal?: number
  queueDone?: number
}
export interface Gateway {
  id: string
  name: string
  connectionId: string
  prefix: string
  port: number
  listenHost: string
  tlsCert: string
  tlsKey: string
  access?: AccessStats
  username: string
  readOnly: boolean
  running: boolean
  url: string
}
export interface Activity {
  id: string | number
  time: string
  kind: string
  status: string
  message: string
  path: string
}
export interface RecoveryEntry {
  id: string
  path: string
  size: number
  created: string
  state: string
  connectionId?: string
  etag?: string
  canRestoreToRemote?: boolean
  integrity?: 'sha256' | 'unverified' | string
}
export interface RecoveryPreview {
  id: string
  connectionId: string
  path: string
  saved: FileEntry
  savedHash: string
  current?: FileEntry
  currentExists: boolean
  expectedCurrentEtag: string
  token: string
}
export interface OperationStatus {
  id: string
  kind: string
  state: string
  sourcePath?: string
  destinationPath?: string
  created: string
  error?: string
}
export interface QueueItem {
  path: string
  kind: string
  state: string
  bytesDone: number
  error: string
  updated: string
}
export interface DownloadTransfer {
  id: string
  connectionId: string
  path: string
  etag: string
  destination: string
  size: number
  received: number
  state: string
  updated: string
}
export interface MultipartUpload {
  operationId: string
  connectionId: string
  path: string
  hash: string
  size: number
  parts: MultipartPart[]
  state: string
}
export interface MultipartPart { number: number; etag: string; size: number }
export interface ObjectVersion {
  entry: FileEntry
  versionId: string
  isLatest: boolean
  deleteMarker: boolean
}
export interface CacheEntry {
  id: string
  connectionId: string
  path: string
  etag: string
  hash: string
  size: number
  pinned: boolean
  updated: string
  localPath: string
  dirty: boolean
  offline: boolean
  remoteChanged: boolean
  checked?: string
}
export interface Preferences {
  rules: ExecutionRules
  bandwidthBytes: number
  maxReaders: number
  stagingBytes: number
  recoveryBytes: number
  cacheBytes: number
  maxFileBytes: number
  recoveryDays: number
  autoStart: boolean
  notifications: boolean
}
export interface ExecutionRules {
  enabled: boolean
  start: string
  end: string
  weekdays: number[]
  networks: string[]
  onlyOnAC: boolean
}
export interface AutomationEnvironment {
  networkInterfaces: string[]
  onAC: boolean
  powerKnown: boolean
}
export interface TransferStatistic {
  scope: string
  id: string
  requests: number
  uploaded: number
  downloaded: number
  errors: number
  lastAccess: string
}
export interface AccessStats { requests: number; errors: number; bytesIn: number; bytesOut: number; active: number }
export interface GatewayAccess {
  time: string
  method: string
  path: string
  status: number
  bytesIn: number
  bytesOut: number
  milliseconds: number
}
export interface DiskInfo {
  stagingBytes: number
  stagingLimit: number
  recoveryBytes: number
  recoveryLimit: number
  freeBytes: number
  cacheBytes?: number
  cacheLimit?: number
}
export interface AppState {
  version: string
  connections: Connection[]
  jobs: Job[]
  gateways: Gateway[]
  activities: Activity[]
  disk: DiskInfo
  dataDir: string
  preferences?: Preferences
}
export interface FileEntry {
  path: string
  name: string
  isDir: boolean
  size: number
  modified: string
  etag: string
  versionId?: string
}
export interface PlanAction {
  size?: number
  path: string
  kind: 'upload' | 'download' | 'conflict' | 'skip' | 'baseline' | 'mkdir-local' | 'mkdir-remote' | 'delete-local' | 'delete-remote' | 'delete-local-dir'
  reason: string
}
export interface JobPreview {
  token: string
  actions: PlanAction[]
  created: string
  uploadBytes: number
  downloadBytes: number
  deleteCount: number
  deletePaths: string[]
  requiresDeleteConfirmation: boolean
}
export interface SyncScanHistory {
  id: number
  jobId: string
  started: string
  completed: string
  scope: { localPath: string; remotePath: string; direction: Direction; exclude: string[] }
  status: 'scanning' | 'success' | 'needs_attention' | 'error' | 'cancelled' | 'interrupted' | string
  error?: string
  localFiles: number
  remoteFiles: number
  localDirectories: number
  remoteDirectories: number
  actions: number
  uploads: number
  downloads: number
  deletes: number
  conflicts: number
  skipped: number
  uploadBytes: number
  downloadBytes: number
}
export interface DeleteTreePreview {
  path: string
  files: FileEntry[]
  directories: string[]
  token: string
}
export interface ConfigurationExport {
  version: number
  backupJobs?: unknown[]
  migrationJobs?: unknown[]
  connections: Omit<Connection, 'tested' | 'capabilities' | 'error'>[]
  jobs: Job[]
  gateways: Gateway[]
  preferences: Preferences
}
