import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Lock } from 'lucide-react';
import { useAuthStore } from '../store/authStore';
import { useToastStore } from '../store/toastStore';
import { authApi } from '../utils/api';

export default function LoginPage() {
  const { t } = useTranslation();
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [loading, setLoading] = useState(false);
  const navigate = useNavigate();
  const authenticated = useAuthStore((s) => s.authenticated);
  const accountSetup = useAuthStore((s) => s.accountSetup);
  const setAuthenticated = useAuthStore((s) => s.setAuthenticated);
  const addToast = useToastStore((s) => s.addToast);

  useEffect(() => {
    if (!accountSetup) {
      navigate('/setup', { replace: true });
      return;
    }
    if (authenticated) {
      navigate('/agents', { replace: true });
    }
  }, [authenticated, accountSetup, navigate]);

  const handleLogin = async () => {
    if (!username.trim() || !password) return;
    setLoading(true);

    try {
      await authApi.login(username.trim(), password);
      setAuthenticated(true);
      navigate('/agents');
    } catch (_err) {
      setAuthenticated(false);
      addToast('error', t('login.invalidCredentials'));
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="min-h-screen bg-gradient-to-br from-indigo-50 to-purple-50 dark:from-gray-950 dark:to-indigo-950 flex items-center justify-center p-4">
      <div className="w-full max-w-md bg-white dark:bg-gray-900 rounded-2xl shadow-xl p-8 space-y-6">
        <div className="text-center">
          <div className="w-16 h-16 rounded-2xl bg-indigo-100 dark:bg-indigo-900 flex items-center justify-center mx-auto mb-4">
            <Lock size={32} className="text-indigo-600 dark:text-indigo-400" />
          </div>
          <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">{t('login.title')}</h1>
          <p className="text-sm text-gray-500 mt-1">{t('login.subtitle')}</p>
        </div>

        <div className="space-y-4">
          <div>
            <label className="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">{t('login.username')}</label>
            <input
              type="text"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              onKeyDown={(e) => e.key === 'Enter' && handleLogin()}
              placeholder={t('login.usernamePlaceholder')}
              className="w-full px-3 py-2.5 text-sm rounded-xl border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
              autoFocus
            />
          </div>

          <div>
            <label className="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">{t('login.password')}</label>
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              onKeyDown={(e) => e.key === 'Enter' && handleLogin()}
              placeholder={t('login.passwordPlaceholder')}
              className="w-full px-3 py-2.5 text-sm rounded-xl border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
            />
          </div>

          <button
            onClick={handleLogin}
            disabled={loading || !username.trim() || !password}
            className="w-full px-4 py-2.5 bg-indigo-600 text-white rounded-xl hover:bg-indigo-700 disabled:opacity-50 text-sm font-medium"
          >
            {loading ? t('login.verifying') : t('login.login')}
          </button>
        </div>
      </div>
    </div>
  );
}
