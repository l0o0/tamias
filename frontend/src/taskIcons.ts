export const taskIconOptions = [
  { id: '', label: '默认' },
  { id: 'folder', label: '文件夹' },
  { id: 'document', label: '文档' },
  { id: 'book', label: '书本' },
  { id: 'graduation', label: '学习' },
  { id: 'code', label: '代码' },
  { id: 'image', label: '图片' },
  { id: 'music', label: '音乐' },
  { id: 'video', label: '视频' },
  { id: 'archive', label: '归档' },
  { id: 'cloud', label: '云端' },
  { id: 'briefcase', label: '工作' },
  { id: 'heart', label: '收藏' },
  { id: 'star', label: '重点' },
  { id: 'squirrel', label: '小花鼠' },
] as const

export type TaskIconName = typeof taskIconOptions[number]['id']

export function normalizeTaskIcon(value: unknown): TaskIconName {
  return taskIconOptions.some((option) => option.id === value) ? value as TaskIconName : ''
}

export function taskIconLabel(value: unknown): string {
  const icon = normalizeTaskIcon(value)
  return taskIconOptions.find((option) => option.id === icon)?.label || '默认'
}
