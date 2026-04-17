import { useState, useEffect } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Plus, X, Star, Trash2, LogOut, Bot, Save, ChevronDown, ChevronRight, Sun, Moon, Monitor, Languages, Copy, KeyRound } from 'lucide-react';
import { agentApi, authApi, modelApi, channelApi, type ProviderInfo, type ConfiguredModel, type ChannelTypeSchema } from '../utils/api';
import { useToastStore } from '../store/toastStore';
import { useAuthStore } from '../store/authStore';
import { useUIStore, type Language } from '../store/uiStore';
import LoadingSpinner from '../components/common/LoadingSpinner';
import SkillWhitelistEditor from '../components/agents/SkillWhitelistEditor';
import LtmCategoryCheckboxGroup from '../components/agents/LtmCategoryCheckboxGroup';
import { DEFAULT_BASE_URLS } from '../constants/providers';
import { COPILOT_AGENT_ID } from '../store/copilotStore';
import type { MemoryConfig, ChannelBinding } from '../types/app';

type CoPilotSettings = {
  skill_whitelist: string[];
  memory: MemoryConfig;
  notification?: { enabled?: boolean; channel_name?: string; granularity?: string };
};

export default function SettingsPage() {
  const { t } = useTranslation();
  const [providers, setProviders] = useState<ProviderInfo[]>([]);
  const [models, setModels] = useState<ConfiguredModel[]>([]);
  const [loading, setLoading] = useState(true);
  const [showAddForm, setShowAddForm] = useState(false);

  const [newProvider, setNewProvider] = useState('');
  const [providerKeyInput, setProviderKeyInput] = useState('');
  const [baseUrl, setBaseUrl] = useState('');
  const [selectedModel, setSelectedModel] = useState('');
  const [customModel, setCustomModel] = useState('');
  const [showCustomInput, setShowCustomInput] = useState(false);
  const [capsNoTemp, setCapsNoTemp] = useState(false);

  const [cpExpanded, setCpExpanded] = useState(false);
  const [cpSettings, setCpSettings] = useState<CoPilotSettings | null>(null);
  const [cpLoading, setCpLoading] = useState(false);
  const [cpNotifEnabled, setCpNotifEnabled] = useState(false);
  const [cpNotifChannel, setCpNotifChannel] = useState('');
  const [cpNotifGranularity, setCpNotifGranularity] = useState<'all' | 'final' | 'failure'>('final');
  const [cpLtmEnabled, setCpLtmEnabled] = useState(false);
  const [cpLtmCategories, setCpLtmCategories] = useState<string[]>([]);
  const [cpLtmMaxEntries, setCpLtmMaxEntries] = useState(200);
  const [cpLtmScopeEnabled, setCpLtmScopeEnabled] = useState(false);
  const [cpLtmGlobalCategories, setCpLtmGlobalCategories] = useState<string[]>([]);
  const [cpLtmPinnedCategories, setCpLtmPinnedCategories] = useState<string[]>([]);
  const [cpLtmRecallCategories, setCpLtmRecallCategories] = useState<string[]>([]);
  const [cpMemMaxMessages, setCpMemMaxMessages] = useState(50);
  const [cpLosslessTokenBudget, setCpLosslessTokenBudget] = useState(4000);
  const [cpLosslessChunkSize, setCpLosslessChunkSize] = useState(10);
  const [cpLosslessMaxDepth, setCpLosslessMaxDepth] = useState(4);
  const [cpLosslessCompactThreshold, setCpLosslessCompactThreshold] = useState(200);
  const [cpLosslessArchiveThreshold, setCpLosslessArchiveThreshold] = useState(1000);
  const [cpSkillWhitelist, setCpSkillWhitelist] = useState<string[]>([]);
  const [cpChannels, setCpChannels] = useState<ChannelBinding[]>([]);
  const [cpChannelTypes, setCpChannelTypes] = useState<ChannelTypeSchema[]>([]);
  const [cpAddChannelType, setCpAddChannelType] = useState('');
  const [cpAddChannelConfig, setCpAddChannelConfig] = useState<Record<string, string>>({});

  const navigate = useNavigate();
  const addToast = useToastStore((s) => s.addToast);
  const setAuthenticated = useAuthStore((s) => s.setAuthenticated);
  const setAccountSetup = useAuthStore((s) => s.setAccountSetup);

  const load = async () => {
    setLoading(true);
    try {
      const [p, m] = await Promise.all([modelApi.getProviders(), modelApi.list()]);
      setProviders(p);
      setModels(m);
    } catch {
      addToast('error', t('settings.loadProvidersFailed'));
    }
    setLoading(false);
  };

  const loadCoPilotSettings = async () => {
    setCpLoading(true);
    try {
      channelApi.types().then(setCpChannelTypes).catch(() => {});
      const agent = await agentApi.get(COPILOT_AGENT_ID);
      const s: CoPilotSettings = {
        skill_whitelist: agent.config.skill_whitelist ?? [],
        memory: agent.config.memory ?? {},
        notification: agent.config.notification,
      };
      setCpSettings(s);
      setCpNotifEnabled(s.notification?.enabled ?? false);
      setCpNotifChannel(s.notification?.channel_name || '');
      setCpNotifGranularity((s.notification?.granularity as 'all' | 'final' | 'failure' | undefined) || 'final');
      setCpMemMaxMessages(s.memory?.max_messages ?? 50);
      setCpLosslessTokenBudget(s.memory?.lossless?.token_budget ?? 4000);
      setCpLosslessChunkSize(s.memory?.lossless?.chunk_size ?? 10);
      setCpLosslessMaxDepth(s.memory?.lossless?.max_depth ?? 4);
      setCpLosslessCompactThreshold(s.memory?.lossless?.compact_threshold ?? 200);
      setCpLosslessArchiveThreshold(s.memory?.lossless?.archive_threshold ?? 1000);
      setCpLtmEnabled(s.memory?.long_term?.enabled ?? false);
      setCpLtmCategories(s.memory?.long_term?.categories ?? []);
      setCpLtmMaxEntries(s.memory?.long_term?.max_entries ?? 200);
      setCpLtmScopeEnabled(s.memory?.long_term?.scope_enabled ?? false);
      setCpLtmGlobalCategories(s.memory?.long_term?.global_categories ?? []);
      setCpLtmPinnedCategories(s.memory?.long_term?.pinned_categories ?? []);
      setCpLtmRecallCategories(s.memory?.long_term?.recall_categories ?? []);
      setCpSkillWhitelist(s.skill_whitelist || []);
      setCpChannels(agent.config.channels ?? []);
    } catch { /* ignore */ }
    setCpLoading(false);
  };

  const handleSaveCoPilot = async () => {
    try {
      const agent = await agentApi.get(COPILOT_AGENT_ID);
      await agentApi.update(COPILOT_AGENT_ID, {
          config: {
          ...agent.config,
          notification: { enabled: cpNotifEnabled, channel_name: cpNotifChannel, granularity: cpNotifGranularity },
          memory: {
            ...agent.config.memory,
            max_messages: cpMemMaxMessages,
            long_term: {
              ...agent.config.memory?.long_term,
              enabled: cpLtmEnabled,
              categories: cpLtmCategories,
              max_entries: cpLtmMaxEntries,
              scope_enabled: cpLtmScopeEnabled,
              global_categories: cpLtmGlobalCategories,
              pinned_categories: cpLtmPinnedCategories,
              recall_categories: cpLtmRecallCategories,
            },
            lossless: {
              token_budget: cpLosslessTokenBudget,
              chunk_size: cpLosslessChunkSize,
              max_depth: cpLosslessMaxDepth,
              compact_threshold: cpLosslessCompactThreshold,
              archive_threshold: cpLosslessArchiveThreshold,
            },
          },
          skill_whitelist: cpSkillWhitelist,
          channels: cpChannels,
        },
      });
      addToast('success', t('settings.copilotSaved'));
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('settings.copilotSaveFailed'));
    }
  };

  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => { load(); }, []);

  useEffect(() => {
    if (window.location.hash === '#copilot' && !cpExpanded) {
      setCpExpanded(true);
      loadCoPilotSettings();
      setTimeout(() => document.getElementById('copilot')?.scrollIntoView({ behavior: 'smooth' }), 100);
    }
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const handleAddModel = async () => {
    if (!newProvider) return;
    const model = showCustomInput ? customModel.trim() : selectedModel;
    if (!model) {
      addToast('error', t('settings.selectModelError'));
      return;
    }
    try {
      const extra: Record<string, unknown> = {};
      if (capsNoTemp) extra.caps = { no_temperature: true };
      await modelApi.add({
        provider: newProvider,
        model,
        api_key: providerKeyInput || undefined,
        base_url: baseUrl || undefined,
        extra: Object.keys(extra).length > 0 ? extra : undefined,
      });
      if (models.length === 0) {
        await modelApi.setDefault(newProvider, model);
      }
      resetForm();
      setShowAddForm(false);
      addToast('success', t('settings.modelAdded'));
      load();
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('settings.modelAddFailed'));
    }
  };

  const handleSetDefault = async (provider: string, model: string) => {
    try {
      await modelApi.setDefault(provider, model);
      addToast('success', t('settings.defaultUpdated'));
      load();
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('settings.defaultFailed'));
    }
  };

  const handleDeleteModel = async (provider: string, model: string) => {
    try {
      await modelApi.delete(`${provider}/${model}`);
      addToast('success', t('settings.modelRemoved'));
      load();
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('settings.removeFailed'));
    }
  };

  const handleLogout = async () => {
    try {
      await authApi.logout();
    } catch {
      // best-effort logout
    } finally {
      setAuthenticated(false);
      setAccountSetup(true);
      navigate('/login');
    }
  };

  const resetForm = () => {
    setNewProvider('');
    setProviderKeyInput('');
    setBaseUrl('');
    setSelectedModel('');
    setCustomModel('');
    setShowCustomInput(false);
    setCapsNoTemp(false);
  };

  const providerNames = providers.map(p => p.name);
  const currentProviderModels = providers.find(p => p.name === newProvider)?.models || [];
  const providerConfigured = providers.find(p => p.name === newProvider)?.configured ?? false;

  if (loading) return <div className="flex justify-center py-16"><LoadingSpinner /></div>;

  return (
    <div className="p-6 max-w-2xl mx-auto space-y-8">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">{t('settings.title')}</h1>
        <button
          onClick={handleLogout}
          className="flex items-center gap-1 px-3 py-1.5 text-sm text-gray-500 hover:text-gray-700 dark:text-gray-400 dark:hover:text-gray-200"
        >
          <LogOut size={14} /> {t('settings.logout')}
        </button>
      </div>

      <ThemeSection />
      <LanguageSection />
      <ChangePasswordSection />

      <section className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4 space-y-3">
        <div className="flex items-center justify-between">
          <h2 className="text-sm font-semibold text-gray-700 dark:text-gray-300">{t('settings.configuredModels')}</h2>
          <button
            onClick={() => { resetForm(); setShowAddForm(true); }}
            className="flex items-center gap-1 px-2 py-1 text-xs bg-indigo-600 text-white rounded hover:bg-indigo-700"
          >
            <Plus size={12} /> {t('settings.addModel')}
          </button>
        </div>

        <div className="space-y-2">
          {models.map((m) => (
            <div key={m.label} className="flex items-center gap-2 p-2 bg-gray-50 dark:bg-gray-800 rounded">
              <button
                onClick={() => handleSetDefault(m.provider, m.model)}
                className={`p-0.5 ${m.is_default ? 'text-amber-500' : 'text-gray-300 hover:text-amber-400'}`}
                title={m.is_default ? t('settings.defaultModel') : t('settings.setDefault')}
              >
                <Star size={14} className={m.is_default ? 'fill-amber-500' : ''} />
              </button>
              <span className="text-sm text-gray-700 dark:text-gray-300 flex-1">
                {m.model}
                <span className="text-xs text-gray-400 ml-2">({m.provider})</span>
                {m.is_default && <span className="text-xs text-amber-600 ml-2">{t('settings.default')}</span>}
              </span>
              {!m.is_default && (
                <button
                  onClick={() => handleDeleteModel(m.provider, m.model)}
                  className="p-1 text-gray-400 hover:text-red-600"
                  title={t('settings.remove')}
                >
                  <Trash2 size={14} />
                </button>
              )}
            </div>
          ))}
          {models.length === 0 && (
            <p className="text-xs text-gray-400 py-2">{t('settings.noModels')}</p>
          )}
        </div>

        {showAddForm && (
          <div className="p-3 bg-indigo-50 dark:bg-indigo-950 rounded-lg space-y-2">
            <div className="flex items-center justify-between">
              <h4 className="text-xs font-medium text-indigo-700 dark:text-indigo-300">{t('settings.addModel')}</h4>
              <button onClick={() => setShowAddForm(false)} className="text-gray-400 hover:text-gray-600">
                <X size={14} />
              </button>
            </div>
            <select
              value={newProvider}
              onChange={(e) => { setNewProvider(e.target.value); setSelectedModel(''); setCustomModel(''); setShowCustomInput(false); }}
              className="w-full px-3 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
            >
              <option value="">{t('settings.selectProvider')}</option>
              {providerNames.map(p => (
                <option key={p} value={p}>{p.charAt(0).toUpperCase() + p.slice(1)}</option>
              ))}
            </select>

            {newProvider && !providerConfigured && (
              <>
                <input
                  value={providerKeyInput}
                  onChange={(e) => setProviderKeyInput(e.target.value)}
                  type="password"
                  placeholder="Provider API Key"
                  className="w-full px-3 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
                />
                <input
                  value={baseUrl}
                  onChange={(e) => setBaseUrl(e.target.value)}
                  placeholder={DEFAULT_BASE_URLS[newProvider] || t('settings.baseUrlOptional')}
                  className="w-full px-3 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
                />
              </>
            )}

            {newProvider && (
              <>
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
                  className="w-full px-3 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
                >
                  <option value="">{t('settings.selectModelPlaceholder')}</option>
                  {currentProviderModels.map((m) => (
                    <option key={m.name} value={m.name}>{m.label} ({m.name})</option>
                  ))}
                  <option value="__custom__">{t('settings.customModelOption')}</option>
                </select>
                {showCustomInput && (
                  <input
                    value={customModel}
                    onChange={(e) => setCustomModel(e.target.value)}
                    placeholder={t('settings.enterCustomModel')}
                    className="w-full px-3 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
                  />
                )}
              </>
            )}

            {showCustomInput && (
              <label className="flex items-center gap-2 text-xs text-gray-600 dark:text-gray-400">
                <input type="checkbox" checked={capsNoTemp} onChange={(e) => setCapsNoTemp(e.target.checked)} className="rounded" />
                {t('settings.capsNoTemperature')}
              </label>
            )}

            <div className="flex gap-2">
              <button onClick={handleAddModel} className="px-3 py-1.5 text-sm bg-indigo-600 text-white rounded hover:bg-indigo-700">
                {t('settings.addModel')}
              </button>
              <button onClick={() => setShowAddForm(false)} className="px-3 py-1.5 text-sm border border-gray-300 dark:border-gray-600 rounded">{t('common.cancel')}</button>
            </div>
          </div>
        )}
      </section>

      {/* CoPilot */}
      <section id="copilot" className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 overflow-hidden">
        <button
          onClick={() => { setCpExpanded(!cpExpanded); if (!cpExpanded && !cpSettings) loadCoPilotSettings(); }}
          className="w-full flex items-center gap-2 p-4 text-left hover:bg-gray-50 dark:hover:bg-gray-800/50 transition-colors"
        >
          {cpExpanded ? <ChevronDown size={16} className="text-gray-400" /> : <ChevronRight size={16} className="text-gray-400" />}
          <Bot size={16} className="text-indigo-500" />
          <h2 className="text-sm font-semibold text-gray-700 dark:text-gray-300 flex-1">{t('settings.copilotSettings')}</h2>
          <span className="text-xs text-gray-400">{t('settings.copilotDesc')}</span>
        </button>

        {cpExpanded && (
          <div className="border-t border-gray-200 dark:border-gray-800 p-4 space-y-5">
            {cpLoading ? (
              <div className="flex justify-center py-8"><LoadingSpinner /></div>
            ) : (
              <>
                {/* Notifications */}
                <div>
                  <h3 className="text-xs font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wider mb-3">{t('settings.notifications')}</h3>
                  <div className="space-y-3">
                    <label className="flex items-center gap-2">
                      <input type="checkbox" checked={cpNotifEnabled} onChange={(e) => setCpNotifEnabled(e.target.checked)} className="rounded text-indigo-600" />
                      <span className="text-sm text-gray-700 dark:text-gray-300">{t('settings.enableNotifications')}</span>
                    </label>
                    {cpNotifEnabled && (
                      <div className="grid grid-cols-2 gap-3">
                        <div>
                          <label className="block text-xs text-gray-500 mb-1">{t('settings.channel')}</label>
                          <select value={cpNotifChannel} onChange={(e) => setCpNotifChannel(e.target.value)} className="w-full px-2 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800">
                            <option value="">{t('settings.webOnly')}</option>
                            {cpChannels.map((c) => <option key={c.type} value={c.type}>{c.type}</option>)}
                          </select>
                        </div>
                        <div>
                          <label className="block text-xs text-gray-500 mb-1">{t('settings.granularity')}</label>
                          <select value={cpNotifGranularity} onChange={(e) => setCpNotifGranularity(e.target.value as 'all' | 'final' | 'failure')} className="w-full px-2 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800">
                            <option value="all">{t('settings.granularityAll')}</option>
                            <option value="final">{t('settings.granularityFinal')}</option>
                            <option value="failure">{t('settings.granularityFailure')}</option>
                          </select>
                        </div>
                      </div>
                    )}
                  </div>
                </div>

                {/* Short-term Memory (Lossless) */}
                <div>
                  <h3 className="text-xs font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wider mb-3">{t('settings.shortTermMemory')}</h3>
                  <div className="space-y-3">
                    <p className="text-[10px] text-gray-400">{t('settings.memLosslessDesc')}</p>
                    <div className="grid grid-cols-2 gap-3 max-w-md">
                      <div>
                        <label className="block text-xs text-gray-500 mb-1">{t('settings.maxMessages')}</label>
                        <input type="number" min={1} value={cpMemMaxMessages} onChange={(e) => setCpMemMaxMessages(Number(e.target.value))} className="w-full px-3 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800" />
                      </div>
                      <div>
                        <label className="block text-xs text-gray-500 mb-1">{t('settings.losslessTokenBudget')}</label>
                        <input type="number" min={1000} value={cpLosslessTokenBudget} onChange={(e) => setCpLosslessTokenBudget(Number(e.target.value))} className="w-full px-3 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800" />
                      </div>
                      <div>
                        <label className="block text-xs text-gray-500 mb-1">{t('settings.losslessChunkSize')}</label>
                        <input type="number" min={3} max={50} value={cpLosslessChunkSize} onChange={(e) => setCpLosslessChunkSize(Number(e.target.value))} className="w-full px-3 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800" />
                      </div>
                      <div>
                        <label className="block text-xs text-gray-500 mb-1">{t('settings.losslessMaxDepth')}</label>
                        <input type="number" min={2} max={6} value={cpLosslessMaxDepth} onChange={(e) => setCpLosslessMaxDepth(Number(e.target.value))} className="w-full px-3 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800" />
                      </div>
                      <div>
                        <label className="block text-xs text-gray-500 mb-1">{t('settings.losslessCompactThreshold')}</label>
                        <input type="number" min={50} value={cpLosslessCompactThreshold} onChange={(e) => setCpLosslessCompactThreshold(Number(e.target.value))} className="w-full px-3 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800" />
                      </div>
                      <div className="col-span-2">
                        <label className="block text-xs text-gray-500 mb-1">{t('settings.losslessArchiveThreshold')}</label>
                        <input type="number" min={100} value={cpLosslessArchiveThreshold} onChange={(e) => setCpLosslessArchiveThreshold(Number(e.target.value))} className="w-full px-3 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800" />
                      </div>
                    </div>
                  </div>
                </div>

                {/* Long-term Memory */}
                <div>
                  <h3 className="text-xs font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wider mb-3">{t('settings.longTermMemory')}</h3>
                  <div className="space-y-3">
                    <label className="flex items-center gap-2">
                      <input type="checkbox" checked={cpLtmEnabled} onChange={(e) => setCpLtmEnabled(e.target.checked)} className="rounded text-indigo-600" />
                      <span className="text-sm text-gray-700 dark:text-gray-300">{t('settings.enableLTM')}</span>
                    </label>
                    {cpLtmEnabled && (
                      <>
                        <LtmCategoryCheckboxGroup
                          label={t('settings.categories')}
                          hint={t('settings.allCategories')}
                          value={cpLtmCategories}
                          onChange={setCpLtmCategories}
                          labelFor={(cat) => t(`agentSettings.ltmCategory.${cat}`)}
                        />
                        <div className="max-w-xs">
                          <label className="block text-xs text-gray-500 mb-1">{t('settings.maxEntries')}</label>
                          <input
                            type="number" min={1}
                            value={cpLtmMaxEntries}
                            onChange={(e) => setCpLtmMaxEntries(Number(e.target.value))}
                            className="w-full px-3 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
                          />
                        </div>
                        <label className="flex items-center gap-2">
                          <input type="checkbox" checked={cpLtmScopeEnabled} onChange={(e) => setCpLtmScopeEnabled(e.target.checked)} className="rounded text-indigo-600" />
                          <span className="text-sm text-gray-700 dark:text-gray-300">{t('settings.scopeEnabled')}</span>
                        </label>
                        <p className="text-[10px] text-gray-400 -mt-2">{t('settings.scopeEnabledHint')}</p>
                        <LtmCategoryCheckboxGroup
                          label={t('settings.globalCategories')}
                          hint={t('settings.globalCategoriesHint')}
                          value={cpLtmGlobalCategories}
                          onChange={setCpLtmGlobalCategories}
                          labelFor={(cat) => t(`agentSettings.ltmCategory.${cat}`)}
                        />
                        <LtmCategoryCheckboxGroup
                          label={t('settings.pinnedCategories')}
                          hint={t('settings.pinnedCategoriesHint')}
                          value={cpLtmPinnedCategories}
                          onChange={setCpLtmPinnedCategories}
                          labelFor={(cat) => t(`agentSettings.ltmCategory.${cat}`)}
                        />
                        <LtmCategoryCheckboxGroup
                          label={t('settings.recallCategories')}
                          hint={t('settings.recallCategoriesHint')}
                          value={cpLtmRecallCategories}
                          onChange={setCpLtmRecallCategories}
                          labelFor={(cat) => t(`agentSettings.ltmCategory.${cat}`)}
                        />
                      </>
                    )}
                  </div>
                </div>

                {/* Skill Whitelist */}
                <div>
                  <h3 className="text-xs font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wider mb-3">{t('settings.skillWhitelist')}</h3>
                  <SkillWhitelistEditor whitelist={cpSkillWhitelist} onChange={setCpSkillWhitelist} />
                </div>

                {/* External Channels */}
                <div>
                  <h3 className="text-xs font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wider mb-3">{t('settings.channels')}</h3>
                  <div className="space-y-3">
                    {cpChannels.map((cb, idx) => {
                      const schema = cpChannelTypes.find((ct) => ct.type === cb.type);
                      return (
                        <div key={idx} className="p-3 rounded-lg border border-gray-200 dark:border-gray-700 space-y-2">
                          <div className="flex items-center justify-between">
                            <span className="text-sm font-medium text-gray-800 dark:text-gray-200">{schema?.label ?? cb.type}</span>
                            <button onClick={() => setCpChannels(cpChannels.filter((_, i) => i !== idx))} className="text-red-500 hover:text-red-700"><Trash2 size={14} /></button>
                          </div>
                          {schema && Object.entries(schema.config_schema).map(([key, field]) => (
                            <div key={key}>
                              <label className="block text-xs text-gray-500 mb-1">{key}{field.required && ' *'}</label>
                              <input
                                type={field.secret ? 'password' : 'text'}
                                value={cb.config[key] ?? ''}
                                onChange={(e) => {
                                  const updated = [...cpChannels];
                                  updated[idx] = { ...cb, config: { ...cb.config, [key]: e.target.value } };
                                  setCpChannels(updated);
                                }}
                                className="w-full px-2 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
                              />
                            </div>
                          ))}
                          <div>
                            <label className="block text-xs text-gray-500 mb-1">{t('settings.webhookUrl')}</label>
                            <div className="flex items-center gap-1">
                              <code className="flex-1 px-2 py-1.5 text-xs bg-gray-100 dark:bg-gray-800 rounded border border-gray-200 dark:border-gray-700 text-gray-600 dark:text-gray-400 truncate">
                                {`${window.location.origin}/api/webhook/${cb.type}`}
                              </code>
                              <button
                                onClick={() => { navigator.clipboard.writeText(`${window.location.origin}/api/webhook/${cb.type}`); }}
                                className="p-1 text-gray-400 hover:text-gray-600"
                              >
                                <Copy size={14} />
                              </button>
                            </div>
                          </div>
                        </div>
                      );
                    })}
                    {cpChannelTypes.length > 0 && (
                      <div className="flex items-end gap-2">
                        <div className="flex-1">
                          <label className="block text-xs text-gray-500 mb-1">{t('settings.addChannel')}</label>
                          <select
                            value={cpAddChannelType}
                            onChange={(e) => { setCpAddChannelType(e.target.value); setCpAddChannelConfig({}); }}
                            className="w-full px-2 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
                          >
                            <option value="">{t('settings.selectChannelType')}</option>
                            {cpChannelTypes
                              .filter((ct) => !cpChannels.some((cb) => cb.type === ct.type))
                              .map((ct) => <option key={ct.type} value={ct.type}>{ct.label}</option>)}
                          </select>
                        </div>
                        <button
                          disabled={!cpAddChannelType}
                          onClick={() => {
                            if (!cpAddChannelType) return;
                            setCpChannels([...cpChannels, { type: cpAddChannelType, config: cpAddChannelConfig }]);
                            setCpAddChannelType('');
                            setCpAddChannelConfig({});
                          }}
                          className="px-3 py-1.5 text-sm bg-indigo-600 text-white rounded hover:bg-indigo-700 disabled:opacity-40 flex items-center gap-1"
                        >
                          <Plus size={14} /> {t('settings.addChannel')}
                        </button>
                      </div>
                    )}
                  </div>
                </div>

                <button
                  onClick={handleSaveCoPilot}
                  className="flex items-center gap-2 px-4 py-2 bg-indigo-600 text-white rounded-lg hover:bg-indigo-700 text-sm font-medium"
                >
                  <Save size={14} /> {t('settings.saveCopilot')}
                </button>
              </>
            )}
          </div>
        )}
      </section>
    </div>
  );
}

const languageOptions: { lang: Language; label: string; desc: string }[] = [
  { lang: 'en', label: 'English', desc: 'Use English interface' },
  { lang: 'zh', label: '中文', desc: '使用中文界面' },
];

function LanguageSection() {
  const { t } = useTranslation();
  const language = useUIStore((s) => s.language);
  const setLanguage = useUIStore((s) => s.setLanguage);

  return (
    <section className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4 space-y-3">
      <h2 className="text-sm font-semibold text-gray-700 dark:text-gray-300 flex items-center gap-2">
        <Languages size={16} className="text-gray-400" /> {t('settings.language')}
      </h2>
      <div className="grid grid-cols-2 gap-3">
        {languageOptions.map(({ lang, label, desc }) => (
          <button
            key={lang}
            onClick={() => setLanguage(lang)}
            className={`flex flex-col items-center gap-2 p-4 rounded-lg border-2 transition-colors ${
              language === lang
                ? 'border-indigo-500 bg-indigo-50 dark:bg-indigo-950'
                : 'border-gray-200 dark:border-gray-700 hover:border-gray-300 dark:hover:border-gray-600'
            }`}
          >
            <span className={`text-sm font-medium ${language === lang ? 'text-indigo-700 dark:text-indigo-300' : 'text-gray-600 dark:text-gray-400'}`}>{label}</span>
            <span className="text-[10px] text-gray-400 text-center leading-tight">{desc}</span>
          </button>
        ))}
      </div>
    </section>
  );
}

function ChangePasswordSection() {
  const { t } = useTranslation();
  const addToast = useToastStore((s) => s.addToast);
  const [currentPassword, setCurrentPassword] = useState('');
  const [newPassword, setNewPassword] = useState('');
  const [confirmNew, setConfirmNew] = useState('');
  const [saving, setSaving] = useState(false);

  const handleChange = async () => {
    if (!currentPassword || !newPassword) return;
    if (newPassword !== confirmNew) {
      addToast('error', t('settings.passwordMismatch'));
      return;
    }
    if (newPassword.length < 6) {
      addToast('error', t('settings.passwordTooShort'));
      return;
    }
    setSaving(true);
    try {
      await authApi.changePassword(currentPassword, newPassword);
      addToast('success', t('settings.passwordChanged'));
      setCurrentPassword('');
      setNewPassword('');
      setConfirmNew('');
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('settings.passwordChangeFailed'));
    } finally {
      setSaving(false);
    }
  };

  return (
    <section className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4 space-y-3">
      <h2 className="text-sm font-semibold text-gray-700 dark:text-gray-300 flex items-center gap-2">
        <KeyRound size={16} className="text-gray-400" /> {t('settings.changePassword')}
      </h2>
      <div className="space-y-3 max-w-sm">
        <div>
          <label className="block text-xs text-gray-500 mb-1">{t('settings.currentPassword')}</label>
          <input
            type="password"
            value={currentPassword}
            onChange={(e) => setCurrentPassword(e.target.value)}
            className="w-full px-3 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
          />
        </div>
        <div>
          <label className="block text-xs text-gray-500 mb-1">{t('settings.newPassword')}</label>
          <input
            type="password"
            value={newPassword}
            onChange={(e) => setNewPassword(e.target.value)}
            className="w-full px-3 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
          />
        </div>
        <div>
          <label className="block text-xs text-gray-500 mb-1">{t('settings.confirmNewPassword')}</label>
          <input
            type="password"
            value={confirmNew}
            onChange={(e) => setConfirmNew(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && handleChange()}
            className="w-full px-3 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
          />
        </div>
        <button
          onClick={handleChange}
          disabled={saving || !currentPassword || !newPassword || !confirmNew}
          className="px-4 py-1.5 text-sm bg-indigo-600 text-white rounded hover:bg-indigo-700 disabled:opacity-50"
        >
          {saving ? t('common.save') + '...' : t('settings.changePassword')}
        </button>
      </div>
    </section>
  );
}

function ThemeSection() {
  const { t } = useTranslation();
  const themeMode = useUIStore((s) => s.themeMode);
  const setThemeMode = useUIStore((s) => s.setThemeMode);

  const themeOptions = [
    { mode: 'light' as const, icon: Sun, label: t('settings.themeLight'), desc: t('settings.themeLightDesc') },
    { mode: 'dark' as const, icon: Moon, label: t('settings.themeDark'), desc: t('settings.themeDarkDesc') },
    { mode: 'system' as const, icon: Monitor, label: t('settings.themeSystem'), desc: t('settings.themeSystemDesc') },
  ];

  return (
    <section className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4 space-y-3">
      <h2 className="text-sm font-semibold text-gray-700 dark:text-gray-300">{t('settings.appearance')}</h2>
      <div className="grid grid-cols-3 gap-3">
        {themeOptions.map(({ mode, icon: Icon, label, desc }) => (
          <button
            key={mode}
            onClick={() => setThemeMode(mode)}
            className={`flex flex-col items-center gap-2 p-4 rounded-lg border-2 transition-colors ${
              themeMode === mode
                ? 'border-indigo-500 bg-indigo-50 dark:bg-indigo-950'
                : 'border-gray-200 dark:border-gray-700 hover:border-gray-300 dark:hover:border-gray-600'
            }`}
          >
            <Icon size={20} className={themeMode === mode ? 'text-indigo-600 dark:text-indigo-400' : 'text-gray-400'} />
            <span className={`text-sm font-medium ${themeMode === mode ? 'text-indigo-700 dark:text-indigo-300' : 'text-gray-600 dark:text-gray-400'}`}>{label}</span>
            <span className="text-[10px] text-gray-400 text-center leading-tight">{desc}</span>
          </button>
        ))}
      </div>
    </section>
  );
}
