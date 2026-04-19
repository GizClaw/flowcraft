import { Component, type ErrorInfo, type ReactNode, useState, useEffect } from 'react';
import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom';

import AgentLayout from './components/layout/AgentLayout';
import AgentDetailLayout from './components/layout/AgentDetailLayout';

import SetupPage from './pages/SetupPage';
import LoginPage from './pages/LoginPage';
import AgentsPage from './pages/AgentsPage';
import AgentEditorPage from './pages/AgentEditorPage';
import AgentLogsPage from './pages/AgentLogsPage';
import AgentRunDetailPage from './pages/AgentRunDetailPage';
import AgentApiPage from './pages/AgentApiPage';
import AgentSettingsPage from './pages/AgentSettingsPage';
import AgentVersionPage from './pages/AgentVersionPage';
import AgentChatPage from './pages/AgentChatPage';
import KanbanPage from './pages/KanbanPage';
import KnowledgePage from './pages/KnowledgePage';
import KnowledgeDetailPage from './pages/KnowledgeDetailPage';
import SkillsPage from './pages/SkillsPage';
import PluginsPage from './pages/PluginsPage';
import MonitoringPage from './pages/MonitoringPage';
import SettingsPage from './pages/SettingsPage';

import ToastContainer from './components/common/ToastContainer';
import CoPilotPanel from './components/copilot/CoPilotPanel';
import { authApi, modelApi } from './utils/api';
import { useAuthStore } from './store/authStore';

class ErrorBoundary extends Component<{ children: ReactNode }, { hasError: boolean; error: Error | null }> {
  constructor(props: { children: ReactNode }) {
    super(props);
    this.state = { hasError: false, error: null };
  }

  static getDerivedStateFromError(error: Error) {
    return { hasError: true, error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('ErrorBoundary caught:', error, info);
  }

  render() {
    if (this.state.hasError) {
      return (
        <div className="flex items-center justify-center h-screen bg-gray-50 dark:bg-gray-950">
          <div className="text-center p-8 max-w-md">
            <h2 className="text-xl font-semibold text-gray-800 dark:text-gray-100 mb-2">Something went wrong</h2>
            <p className="text-gray-500 dark:text-gray-400 mb-4">{this.state.error?.message}</p>
            <button
              onClick={() => this.setState({ hasError: false, error: null })}
              className="px-4 py-2 bg-indigo-600 text-white rounded-lg hover:bg-indigo-700"
            >
              Try again
            </button>
          </div>
        </div>
      );
    }
    return this.props.children;
  }
}

function SetupGuard({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<'loading' | 'configured' | 'not_configured'>('loading');
  const authenticated = useAuthStore((s) => s.authenticated);

  useEffect(() => {
    if (!authenticated) return;
    let cancelled = false;
    modelApi.getProviders()
      .then((providers) => {
        if (cancelled) return;
        const hasConfigured = providers.some((p) => p.configured);
        setStatus(hasConfigured ? 'configured' : 'not_configured');
      })
      .catch(() => {
        if (!cancelled) setStatus('not_configured');
      });
    return () => { cancelled = true; };
  }, [authenticated]);

  if (status === 'loading') {
    return (
      <div className="flex items-center justify-center h-screen bg-gray-50 dark:bg-gray-950">
        <div className="text-gray-400 text-sm">Loading...</div>
      </div>
    );
  }

  if (status === 'not_configured') {
    return <Navigate to="/setup" replace />;
  }

  return <>{children}</>;
}

function AuthGuard({ children }: { children: ReactNode }) {
  const loading = useAuthStore((s) => s.loading);
  const accountSetup = useAuthStore((s) => s.accountSetup);
  const authenticated = useAuthStore((s) => s.authenticated);

  if (loading) {
    return (
      <div className="flex items-center justify-center h-screen bg-gray-50 dark:bg-gray-950">
        <div className="text-gray-400 text-sm">Loading...</div>
      </div>
    );
  }

  if (!accountSetup) {
    return <Navigate to="/setup" replace />;
  }

  if (!authenticated) {
    return <Navigate to="/login" replace />;
  }

  return <>{children}</>;
}

export default function App() {
  const setAuthenticated = useAuthStore((s) => s.setAuthenticated);
  const setAccountSetup = useAuthStore((s) => s.setAccountSetup);
  const setLoading = useAuthStore((s) => s.setLoading);

  useEffect(() => {
    let cancelled = false;
    authApi.status()
      .then((status) => {
        if (cancelled) return;
        setAccountSetup(status.initialized ?? false);
        setAuthenticated(status.authenticated ?? false);
        setLoading(false);
      })
      .catch(() => {
        if (cancelled) return;
        setAccountSetup(false);
        setAuthenticated(false);
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [setAuthenticated, setAccountSetup, setLoading]);

  return (
    <ErrorBoundary>
      <ToastContainer />
      <BrowserRouter>
        <Routes>
          <Route path="/login" element={<LoginPage />} />
          <Route path="/setup" element={<SetupPage />} />

          <Route
            element={
              <AuthGuard>
                <SetupGuard>
                  <CoPilotPanel />
                  <AgentLayout />
                </SetupGuard>
              </AuthGuard>
            }
          >
            <Route index element={<Navigate to="/agents" replace />} />

            <Route path="agents" element={<AgentsPage />} />
            <Route path="agents/:id" element={<AgentDetailLayout />}>
              <Route index element={<Navigate to="editor" replace />} />
              <Route path="editor" element={<AgentEditorPage />} />
              <Route path="chat" element={<AgentChatPage />} />
              <Route path="logs" element={<AgentLogsPage />} />
              <Route path="runs/:runId" element={<AgentRunDetailPage />} />
              <Route path="api" element={<AgentApiPage />} />
              <Route path="settings-app" element={<AgentSettingsPage />} />
              <Route path="versions" element={<AgentVersionPage />} />
            </Route>

            <Route path="kanban" element={<KanbanPage />} />
            <Route path="knowledge" element={<KnowledgePage />} />
            <Route path="knowledge/:id" element={<KnowledgeDetailPage />} />
            <Route path="skills" element={<SkillsPage />} />
            <Route path="plugins" element={<PluginsPage />} />
            <Route path="monitoring" element={<MonitoringPage />} />
            <Route path="global-settings" element={<SettingsPage />} />

            <Route path="*" element={<Navigate to="/agents" replace />} />
          </Route>
        </Routes>
      </BrowserRouter>
    </ErrorBoundary>
  );
}
