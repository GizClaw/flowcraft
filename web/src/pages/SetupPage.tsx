import { useState, useEffect } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Sparkles, AlertCircle, UserPlus } from 'lucide-react';
import { authApi, modelApi, type ProviderInfo } from '../utils/api';
import { useToastStore } from '../store/toastStore';
import { useAuthStore } from '../store/authStore';
import { DEFAULT_BASE_URLS } from '../constants/providers';

type Step = 'account' | 'provider';

export default function SetupPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const addToast = useToastStore((s) => s.addToast);
  const accountSetup = useAuthStore((s) => s.accountSetup);
  const setAccountSetup = useAuthStore((s) => s.setAccountSetup);
  const setAuthenticated = useAuthStore((s) => s.setAuthenticated);

  const [step, setStep] = useState<Step>(accountSetup ? 'provider' : 'account');

  // Account step state
  const [adminUsername, setAdminUsername] = useState('');
  const [adminPassword, setAdminPassword] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const [creatingAccount, setCreatingAccount] = useState(false);

  // Provider step state
  const [provider, setProvider] = useState('');
  const [providerKey, setProviderKey] = useState('');
  const [baseUrl, setBaseUrl] = useState('');
  const [selectedModel, setSelectedModel] = useState('');
  const [customModel, setCustomModel] = useState('');
  const [showCustomInput, setShowCustomInput] = useState(false);
  const [capsNoTemp, setCapsNoTemp] = useState(false);
  const [providers, setProviders] = useState<ProviderInfo[]>([]);
  const [loading, setLoading] = useState(false);
  const [loadError, setLoadError] = useState(false);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (step === 'provider') {
      setLoading(true);
      modelApi.getProviders()
        .then((p) => {
          setProviders(p);
          setProvider((prev) => prev || (p.length > 0 ? p[0].name : ''));
          setLoadError(false);
        })
        .catch(() => setLoadError(true))
        .finally(() => setLoading(false));
    }
  }, [step]);

  const providerNames = providers.map(p => p.name);
  const currentProviderModels = providers.find(p => p.name === provider)?.models || [];

  const handleCreateAccount = async () => {
    if (!adminUsername.trim() || !adminPassword) return;
    if (adminPassword !== confirmPassword) {
      addToast('error', t('setup.passwordMismatch'));
      return;
    }
    if (adminPassword.length < 6) {
      addToast('error', t('setup.passwordTooShort'));
      return;
    }
    setCreatingAccount(true);
    try {
      await authApi.setup(adminUsername.trim(), adminPassword);
      await authApi.login(adminUsername.trim(), adminPassword);
      setAccountSetup(true);
      setAuthenticated(true);
      setStep('provider');
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('setup.setupFailed'));
    } finally {
      setCreatingAccount(false);
    }
  };

  const handleProviderSetup = async () => {
    if (!providerKey.trim() && provider !== 'ollama') return;
    const model = showCustomInput ? customModel.trim() : selectedModel;
    if (!model) {
      addToast('error', t('setup.selectModelError'));
      return;
    }
    setSaving(true);
    try {
      const extra: Record<string, unknown> = {};
      if (capsNoTemp) extra.caps = { no_temperature: true };
      await modelApi.add({
        provider,
        model,
        api_key: providerKey.trim() || undefined,
        base_url: baseUrl.trim() || undefined,
        extra: Object.keys(extra).length > 0 ? extra : undefined,
      });
      await modelApi.setDefault(provider, model);
      addToast('success', t('setup.configured'));
      navigate('/agents');
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('setup.setupFailed'));
    } finally {
      setSaving(false);
    }
  };

  if (step === 'account') {
    return (
      <div className="min-h-screen bg-gradient-to-br from-indigo-50 to-purple-50 dark:from-gray-950 dark:to-indigo-950 flex items-center justify-center p-4">
        <div className="w-full max-w-md bg-white dark:bg-gray-900 rounded-2xl shadow-xl p-8 space-y-6">
          <div className="text-center">
            <div className="w-16 h-16 rounded-2xl bg-indigo-100 dark:bg-indigo-900 flex items-center justify-center mx-auto mb-4">
              <UserPlus size={32} className="text-indigo-600 dark:text-indigo-400" />
            </div>
            <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">{t('setup.title')}</h1>
            <p className="text-sm text-gray-500 mt-1">{t('setup.accountSubtitle')}</p>
          </div>

          <div className="space-y-4">
            <div>
              <label className="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">{t('setup.adminUsername')}</label>
              <input
                type="text"
                value={adminUsername}
                onChange={(e) => setAdminUsername(e.target.value)}
                onKeyDown={(e) => e.key === 'Enter' && handleCreateAccount()}
                placeholder={t('setup.adminUsernamePlaceholder')}
                className="w-full px-3 py-2.5 text-sm rounded-xl border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
                autoFocus
              />
            </div>

            <div>
              <label className="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">{t('setup.adminPassword')}</label>
              <input
                type="password"
                value={adminPassword}
                onChange={(e) => setAdminPassword(e.target.value)}
                onKeyDown={(e) => e.key === 'Enter' && handleCreateAccount()}
                placeholder={t('setup.adminPasswordPlaceholder')}
                className="w-full px-3 py-2.5 text-sm rounded-xl border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
              />
            </div>

            <div>
              <label className="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">{t('setup.confirmPassword')}</label>
              <input
                type="password"
                value={confirmPassword}
                onChange={(e) => setConfirmPassword(e.target.value)}
                onKeyDown={(e) => e.key === 'Enter' && handleCreateAccount()}
                placeholder={t('setup.confirmPasswordPlaceholder')}
                className="w-full px-3 py-2.5 text-sm rounded-xl border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
              />
            </div>

            <button
              onClick={handleCreateAccount}
              disabled={creatingAccount || !adminUsername.trim() || !adminPassword || !confirmPassword}
              className="w-full px-4 py-2.5 bg-indigo-600 text-white rounded-xl hover:bg-indigo-700 disabled:opacity-50 text-sm font-medium"
            >
              {creatingAccount ? t('setup.creatingAccount') : t('setup.createAccount')}
            </button>
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="min-h-screen bg-gradient-to-br from-indigo-50 to-purple-50 dark:from-gray-950 dark:to-indigo-950 flex items-center justify-center p-4">
      <div className="w-full max-w-md bg-white dark:bg-gray-900 rounded-2xl shadow-xl p-8 space-y-6">
        <div className="text-center">
          <div className="w-16 h-16 rounded-2xl bg-indigo-100 dark:bg-indigo-900 flex items-center justify-center mx-auto mb-4">
            <Sparkles size={32} className="text-indigo-600 dark:text-indigo-400" />
          </div>
          <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">{t('setup.providerTitle')}</h1>
          <p className="text-sm text-gray-500 mt-1">{t('setup.subtitle')}</p>
        </div>

        {loading ? (
          <p className="text-sm text-gray-500 text-center py-4">{t('setup.loadingProviders')}</p>
        ) : loadError ? (
          <div className="flex flex-col items-center gap-3 py-4">
            <AlertCircle size={32} className="text-red-400" />
            <p className="text-sm text-red-600 dark:text-red-400 text-center">{t('setup.loadProvidersFailed')}</p>
            <button
              onClick={() => window.location.reload()}
              className="px-4 py-2 text-sm bg-indigo-600 text-white rounded-xl hover:bg-indigo-700"
            >
              {t('common.retry')}
            </button>
          </div>
        ) : (
        <div className="space-y-4">
          <div>
            <label className="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">{t('setup.provider')}</label>
            <select
              value={provider}
              onChange={(e) => { setProvider(e.target.value); setSelectedModel(''); setCustomModel(''); setShowCustomInput(false); }}
              className="w-full px-3 py-2.5 text-sm rounded-xl border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
            >
              {providerNames.map((p) => <option key={p} value={p}>{p.charAt(0).toUpperCase() + p.slice(1)}</option>)}
            </select>
          </div>

          <div>
            <label className="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">{t('setup.providerApiKey')}</label>
            <input
              type="password"
              value={providerKey}
              onChange={(e) => setProviderKey(e.target.value)}
              placeholder={provider === 'ollama' ? t('setup.ollamaOptional') : 'sk-...'}
              className="w-full px-3 py-2.5 text-sm rounded-xl border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
            />
          </div>

          {(provider === 'ollama' || provider === 'deepseek' || provider === 'azure' || provider === 'minimax' || provider === 'qwen' || provider === 'bytedance') && (
            <div>
              <label className="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">{t('setup.baseUrl')}</label>
              <input
                value={baseUrl}
                onChange={(e) => setBaseUrl(e.target.value)}
                placeholder={DEFAULT_BASE_URLS[provider] || 'https://api.example.com/v1'}
                className="w-full px-3 py-2.5 text-sm rounded-xl border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
              />
            </div>
          )}

          <div>
            <label className="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">{t('setup.model')}</label>
            <select
              value={showCustomInput ? '__custom__' : selectedModel}
              onChange={(e) => {
                if (e.target.value === '__custom__') {
                  setSelectedModel('');
                  setCustomModel('');
                  setShowCustomInput(true);
                } else {
                  setSelectedModel(e.target.value);
                  setCustomModel('');
                  setShowCustomInput(false);
                }
              }}
              className="w-full px-3 py-2.5 text-sm rounded-xl border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
            >
              <option value="">{t('setup.selectModel')}</option>
              {currentProviderModels.map((m) => (
                <option key={m.name} value={m.name}>{m.label} ({m.name})</option>
              ))}
              <option value="__custom__">{t('setup.customModel')}</option>
            </select>
          </div>
          {showCustomInput && (
            <>
              <input
                value={customModel}
                onChange={(e) => setCustomModel(e.target.value)}
                placeholder={t('setup.enterCustomModel')}
                className="w-full px-3 py-2.5 text-sm rounded-xl border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
              />
              <label className="flex items-center gap-2 text-xs text-gray-600 dark:text-gray-400">
                <input type="checkbox" checked={capsNoTemp} onChange={(e) => setCapsNoTemp(e.target.checked)} className="rounded" />
                {t('settings.capsNoTemperature')}
              </label>
            </>
          )}

          <button
            onClick={handleProviderSetup}
            disabled={saving || (!providerKey.trim() && provider !== 'ollama')}
            className="w-full px-4 py-2.5 bg-indigo-600 text-white rounded-xl hover:bg-indigo-700 disabled:opacity-50 text-sm font-medium"
          >
            {saving ? t('setup.configuring') : t('setup.getStarted')}
          </button>
        </div>
        )}
      </div>
    </div>
  );
}
