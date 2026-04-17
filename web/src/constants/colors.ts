export const STATUS_COLORS: Record<string, string> = {
  running: 'border-blue-500 shadow-blue-200 dark:shadow-blue-900',
  completed: 'border-green-500',
  error: 'border-red-500',
  skipped: 'border-gray-300 opacity-60',
  idle: 'border-gray-200 dark:border-gray-700',
};

export const KANBAN_COLUMN_COLORS: Record<string, string> = {
  pending: 'bg-gray-50 dark:bg-gray-800',
  claimed: 'bg-blue-50 dark:bg-blue-950',
  done: 'bg-green-50 dark:bg-green-950',
  failed: 'bg-red-50 dark:bg-red-950',
};

export const minimapColorMap: Record<string, string> = {
  'bg-blue-50': '#dbeafe',
  'bg-green-50': '#dcfce7',
  'bg-orange-50': '#fff7ed',
  'bg-purple-50': '#faf5ff',
  'bg-pink-50': '#fdf2f8',
  'bg-cyan-50': '#ecfeff',
  'bg-indigo-50': '#eef2ff',
  'bg-yellow-50': '#fefce8',
  'bg-teal-50': '#f0fdfa',
  'bg-rose-50': '#fff1f2',
  'bg-emerald-50': '#ecfdf5',
  'bg-amber-50': '#fffbeb',
  'bg-lime-50': '#f7fee7',
  'bg-red-50': '#fef2f2',
  'bg-violet-50': '#f5f3ff',
  'bg-sky-50': '#f0f9ff',
  'bg-gray-50': '#f9fafb',
};
