import { Outlet, NavLink } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Menu, Bell, X } from 'lucide-react';
import * as icons from 'lucide-react';
import { useUIStore } from '../../store/uiStore';
import { useNotificationStore } from '../../store/notificationStore';
import { mainNavItems } from '../../constants/navigation';
import NotificationPanel from '../common/NotificationPanel';
import { useState, useEffect } from 'react';

export default function AgentLayout() {
  const { t } = useTranslation();
  const sidebarOpen = useUIStore((s) => s.sidebarOpen);
  const toggleSidebar = useUIStore((s) => s.toggleSidebar);
  const unreadCount = useNotificationStore((s) => s.unreadCount);
  const [showNotifications, setShowNotifications] = useState(false);
  const [isMobile, setIsMobile] = useState(false);

  useEffect(() => {
    const mq = window.matchMedia('(max-width: 768px)');
    const handler = (e: MediaQueryListEvent | MediaQueryList) => setIsMobile(e.matches);
    handler(mq);
    mq.addEventListener('change', handler);
    return () => mq.removeEventListener('change', handler);
  }, []);

  const sidebarVisible = isMobile ? sidebarOpen : true;
  const sidebarExpanded = isMobile ? true : sidebarOpen;

  return (
    <div className="flex h-screen bg-gray-50 dark:bg-gray-950">
      {isMobile && sidebarOpen && (
        <div className="fixed inset-0 bg-black/40 z-40" onClick={toggleSidebar} />
      )}

      <aside className={`
        ${isMobile ? 'fixed inset-y-0 left-0 z-50' : 'relative'}
        ${sidebarVisible ? (sidebarExpanded ? 'w-56' : 'w-16') : 'w-0 overflow-hidden'}
        flex flex-col border-r border-gray-200 dark:border-gray-800 bg-white dark:bg-gray-900 transition-all duration-200 shrink-0
      `}>
        <div className="flex items-center gap-2 px-4 h-14 border-b border-gray-200 dark:border-gray-800">
          {isMobile ? (
            <button onClick={toggleSidebar} className="p-1.5 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800">
              <X size={18} />
            </button>
          ) : (
            <button onClick={toggleSidebar} className="p-1.5 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800">
              <Menu size={18} />
            </button>
          )}
          {sidebarExpanded && <span className="font-bold text-indigo-600 dark:text-indigo-400 text-lg whitespace-nowrap">FlowCraft</span>}
        </div>
        <nav className="flex-1 py-2 overflow-y-auto">
          {mainNavItems.map((item) => {
            const Icon = (icons as unknown as Record<string, React.FC<{ size?: number }>>)[item.icon] || icons.Circle;
            return (
              <NavLink
                key={item.path}
                to={item.path}
                onClick={() => isMobile && toggleSidebar()}
                className={({ isActive }) =>
                  `flex items-center gap-3 px-4 py-2.5 mx-2 rounded-lg text-sm transition-colors whitespace-nowrap ${
                    isActive ? 'bg-indigo-50 dark:bg-indigo-950 text-indigo-700 dark:text-indigo-300 font-medium' : 'text-gray-600 dark:text-gray-400 hover:bg-gray-100 dark:hover:bg-gray-800'
                  }`
                }
              >
                <Icon size={18} />
                {sidebarExpanded && <span>{t(item.labelKey)}</span>}
              </NavLink>
            );
          })}
        </nav>
      </aside>

      <div className="flex-1 flex flex-col min-w-0">
        <header className="h-14 flex items-center justify-between px-4 border-b border-gray-200 dark:border-gray-800 bg-white dark:bg-gray-900 shrink-0">
          <div>
            {isMobile && !sidebarOpen && (
              <button onClick={toggleSidebar} className="p-1.5 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800 text-gray-500">
                <Menu size={18} />
              </button>
            )}
          </div>
          <div className="relative">
            <button
              onClick={() => setShowNotifications(!showNotifications)}
              className="p-2 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800 text-gray-500 relative"
            >
              <Bell size={18} />
              {unreadCount > 0 && (
                <span className="absolute top-1 right-1 w-4 h-4 bg-red-500 text-white text-[10px] rounded-full flex items-center justify-center font-bold">
                  {unreadCount > 9 ? '9+' : unreadCount}
                </span>
              )}
            </button>
            {showNotifications && (
              <NotificationPanel onClose={() => setShowNotifications(false)} />
            )}
          </div>
        </header>
        <main className="flex-1 overflow-auto">
          <Outlet />
        </main>
      </div>
    </div>
  );
}
