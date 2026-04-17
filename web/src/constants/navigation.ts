export interface NavItem {
  labelKey: string;
  path: string;
  icon: string;
}

export const mainNavItems: NavItem[] = [
  { labelKey: 'nav.agents', path: '/agents', icon: 'LayoutGrid' },
  { labelKey: 'nav.knowledge', path: '/knowledge', icon: 'BookOpen' },
  { labelKey: 'nav.kanban', path: '/kanban', icon: 'Kanban' },
  { labelKey: 'nav.skills', path: '/skills', icon: 'Puzzle' },
  { labelKey: 'nav.plugins', path: '/plugins', icon: 'Plug' },
  { labelKey: 'nav.monitoring', path: '/monitoring', icon: 'BarChart3' },
  { labelKey: 'nav.settings', path: '/global-settings', icon: 'Settings' },
];

export const agentDetailTabs: NavItem[] = [
  { labelKey: 'agentTabs.editor', path: 'editor', icon: 'PenTool' },
  { labelKey: 'agentTabs.chat', path: 'chat', icon: 'MessageCircle' },
  { labelKey: 'agentTabs.logs', path: 'logs', icon: 'ScrollText' },
  { labelKey: 'agentTabs.versions', path: 'versions', icon: 'GitBranch' },
  { labelKey: 'agentTabs.api', path: 'api', icon: 'Code' },
  { labelKey: 'agentTabs.settings', path: 'settings-app', icon: 'Settings' },
];
