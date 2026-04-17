export interface PluginInfo {
  id: string;
  name: string;
  version: string;
  type: string;
  description?: string;
  author?: string;
  icon?: string;
  builtin: boolean;
}

export interface Plugin {
  info: PluginInfo;
  status: 'installed' | 'active' | 'inactive' | 'error';
  config?: Record<string, unknown>;
  error?: string;
}
