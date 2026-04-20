import { useState, useEffect } from 'react';
import { useOutletContext } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Save, Plus, Trash2, Copy } from 'lucide-react';
import { agentApi, channelApi, type ChannelTypeSchema } from '../utils/api';
import SchemaEditor from '../components/variable/SchemaEditor';
import SkillWhitelistEditor from '../components/agents/SkillWhitelistEditor';
import LtmCategoryCheckboxGroup from '../components/agents/LtmCategoryCheckboxGroup';
import AgentMemoryPage from './AgentMemoryPage';
import { useToastStore } from '../store/toastStore';
import type { Agent, UpdateAgentRequest, ChannelBinding } from '../types/app';
import type { VariableSchema } from '../types/variable';

export default function AgentSettingsPage() {
  const { t } = useTranslation();
  const { agent, setAgent } = useOutletContext<{ agent: Agent; setAgent: (a: Agent) => void }>();
  const [name, setName] = useState(agent.name);
  const [description, setDescription] = useState(agent.description || '');
  const [inputSchema, setInputSchema] = useState<VariableSchema>(agent.input_schema || { variables: [] });
  const [outputSchema, setOutputSchema] = useState<VariableSchema>(agent.output_schema || { variables: [] });
  const [skillWhitelist, setSkillWhitelist] = useState<string[]>(agent.config?.skill_whitelist || []);
  const [parallelEnabled, setParallelEnabled] = useState(agent.config?.parallel?.enabled ?? true);
  const [mergeStrategy, setMergeStrategy] = useState(agent.config?.parallel?.merge_strategy || 'last_wins');
  const [maxBranches, setMaxBranches] = useState(agent.config?.parallel?.max_branches ?? 10);
  const [maxNesting, setMaxNesting] = useState(agent.config?.parallel?.max_nesting ?? 3);
  const [notifEnabled, setNotifEnabled] = useState(agent.config?.notification?.enabled ?? false);
  const [notifChannel, setNotifChannel] = useState(agent.config?.notification?.channel_name || '');
  const [notifGranularity, setNotifGranularity] = useState(agent.config?.notification?.granularity || 'final');
  const [ltmEnabled, setLtmEnabled] = useState(agent.config?.memory?.long_term?.enabled ?? false);
  const [ltmCategories, setLtmCategories] = useState<string[]>(agent.config?.memory?.long_term?.categories ?? []);
  const [ltmMaxEntries, setLtmMaxEntries] = useState(agent.config?.memory?.long_term?.max_entries ?? 100);
  const [ltmScopeEnabled, setLtmScopeEnabled] = useState(agent.config?.memory?.long_term?.scope_enabled ?? false);
  const [ltmGlobalCategories, setLtmGlobalCategories] = useState<string[]>(agent.config?.memory?.long_term?.global_categories ?? []);
  const [ltmPinnedCategories, setLtmPinnedCategories] = useState<string[]>(agent.config?.memory?.long_term?.pinned_categories ?? []);
  const [ltmRecallCategories, setLtmRecallCategories] = useState<string[]>(agent.config?.memory?.long_term?.recall_categories ?? []);
  const [channelBindings, setChannelBindings] = useState<ChannelBinding[]>(agent.config?.channels ?? []);
  const [channelTypes, setChannelTypes] = useState<ChannelTypeSchema[]>([]);
  const [addChannelType, setAddChannelType] = useState('');
  const [addChannelConfig, setAddChannelConfig] = useState<Record<string, string>>({});
  const addToast = useToastStore((s) => s.addToast);

  useEffect(() => {
    channelApi.types().then(setChannelTypes).catch(() => {});
  }, []);

  const handleSave = async () => {
    // Mirrors SettingsPage: when notifications are off, persist only the
    // disabled flag rather than carrying the (now-meaningless) channel and
    // granularity selections forward.
    const notification = notifEnabled
      ? { enabled: true, channel_name: notifChannel, granularity: notifGranularity as 'all' | 'final' | 'failure' }
      : { enabled: false };
    const update: UpdateAgentRequest = {
      name, description,
      input_schema: inputSchema,
      output_schema: outputSchema,
      config: {
        ...agent.config,
        skill_whitelist: skillWhitelist,
        parallel: { enabled: parallelEnabled, merge_strategy: mergeStrategy as 'last_wins', max_branches: maxBranches, max_nesting: maxNesting },
        notification,
        memory: {
          ...agent.config?.memory,
          long_term: {
            ...agent.config?.memory?.long_term,
            enabled: ltmEnabled,
            categories: ltmCategories,
            max_entries: ltmMaxEntries,
            scope_enabled: ltmScopeEnabled,
            global_categories: ltmGlobalCategories,
            pinned_categories: ltmPinnedCategories,
            recall_categories: ltmRecallCategories,
          },
        },
        channels: channelBindings,
      },
    };
    try {
      const updated = await agentApi.update(agent.id, update);
      setAgent(updated);
      addToast('success', t('agentSettings.saved'));
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('agentSettings.saveFailed'));
    }
  };

  return (
    <div className="p-6 max-w-2xl mx-auto space-y-8">
      <section>
        <h2 className="text-lg font-semibold text-gray-900 dark:text-gray-100 mb-4">{t('agentSettings.general')}</h2>
        <div className="space-y-3">
          <div>
            <label className="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">{t('agentSettings.name')}</label>
            <input value={name} onChange={(e) => setName(e.target.value)} className="w-full px-3 py-2 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800" />
          </div>
          <div>
            <label className="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">{t('agentSettings.description')}</label>
            <input value={description} onChange={(e) => setDescription(e.target.value)} className="w-full px-3 py-2 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800" />
          </div>
        </div>
      </section>

      <section>
        <h2 className="text-lg font-semibold text-gray-900 dark:text-gray-100 mb-4">{t('agentSettings.inputSchema')}</h2>
        <SchemaEditor schema={inputSchema} onChange={setInputSchema} />
      </section>

      <section>
        <h2 className="text-lg font-semibold text-gray-900 dark:text-gray-100 mb-4">{t('agentSettings.outputSchema')}</h2>
        <SchemaEditor schema={outputSchema} onChange={setOutputSchema} />
      </section>

      <section>
        <h2 className="text-lg font-semibold text-gray-900 dark:text-gray-100 mb-4">{t('agentSettings.parallelExecution')}</h2>
        <div className="space-y-3">
          <label className="flex items-center gap-2"><input type="checkbox" checked={parallelEnabled} onChange={(e) => setParallelEnabled(e.target.checked)} className="rounded text-indigo-600" /><span className="text-sm text-gray-700 dark:text-gray-300">{t('agentSettings.enableParallel')}</span></label>
          {parallelEnabled && (
            <div className="grid grid-cols-3 gap-3">
              <div>
                <label className="block text-xs text-gray-500 mb-1">{t('agentSettings.mergeStrategy')}</label>
                <select value={mergeStrategy} onChange={(e) => setMergeStrategy(e.target.value as 'last_wins' | 'namespace' | 'error_on_conflict')} className="w-full px-2 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800">
                  <option value="last_wins">{t('agentSettings.lastWins')}</option>
                  <option value="namespace">{t('agentSettings.namespace')}</option>
                  <option value="error_on_conflict">{t('agentSettings.errorOnConflict')}</option>
                </select>
              </div>
              <div>
                <label className="block text-xs text-gray-500 mb-1">{t('agentSettings.maxBranches')}</label>
                <input type="number" value={maxBranches} onChange={(e) => setMaxBranches(Number(e.target.value))} className="w-full px-2 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800" />
              </div>
              <div>
                <label className="block text-xs text-gray-500 mb-1">{t('agentSettings.maxNesting')}</label>
                <input type="number" value={maxNesting} onChange={(e) => setMaxNesting(Number(e.target.value))} className="w-full px-2 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800" />
              </div>
            </div>
          )}
        </div>
      </section>

      <section>
        <h2 className="text-lg font-semibold text-gray-900 dark:text-gray-100 mb-4">{t('agentSettings.notifications')}</h2>
        <div className="space-y-3">
          <label className="flex items-center gap-2"><input type="checkbox" checked={notifEnabled} onChange={(e) => setNotifEnabled(e.target.checked)} className="rounded text-indigo-600" /><span className="text-sm text-gray-700 dark:text-gray-300">{t('agentSettings.enableNotifications')}</span></label>
          {notifEnabled && (
            <div className="grid grid-cols-2 gap-3">
              <div>
                <label className="block text-xs text-gray-500 mb-1">{t('agentSettings.channel')}</label>
                <select value={notifChannel} onChange={(e) => setNotifChannel(e.target.value)} className="w-full px-2 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800">
                  <option value="">{t('agentSettings.webOnly')}</option>
                  {channelBindings.map((cb) => <option key={cb.type} value={cb.type}>{cb.type}</option>)}
                </select>
                {channelBindings.length === 0 && <p className="text-[10px] text-gray-400 mt-0.5">{t('agentSettings.noChannels')}</p>}
              </div>
              <div>
                <label className="block text-xs text-gray-500 mb-1">{t('agentSettings.granularity')}</label>
                <select value={notifGranularity} onChange={(e) => setNotifGranularity(e.target.value as 'all' | 'final' | 'failure')} className="w-full px-2 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800">
                  <option value="all">{t('agentSettings.granularityAll')}</option>
                  <option value="final">{t('agentSettings.granularityFinal')}</option>
                  <option value="failure">{t('agentSettings.granularityFailure')}</option>
                </select>
              </div>
            </div>
          )}
        </div>
      </section>

      <section>
        <h2 className="text-lg font-semibold text-gray-900 dark:text-gray-100 mb-4">{t('agentSettings.longTermMemory')}</h2>
        <div className="space-y-3">
          <label className="flex items-center gap-2"><input type="checkbox" checked={ltmEnabled} onChange={(e) => setLtmEnabled(e.target.checked)} className="rounded text-indigo-600" /><span className="text-sm text-gray-700 dark:text-gray-300">{t('agentSettings.enableLTM')}</span></label>
          {ltmEnabled && (
            <>
              <LtmCategoryCheckboxGroup
                label={t('agentSettings.categories')}
                hint={t('agentSettings.allCategories')}
                value={ltmCategories}
                onChange={setLtmCategories}
                labelFor={(cat) => t(`agentSettings.ltmCategory.${cat}`)}
              />
              <div className="max-w-xs">
                <label className="block text-xs text-gray-500 mb-1">{t('agentSettings.maxEntries')}</label>
                <input type="number" value={ltmMaxEntries} onChange={(e) => setLtmMaxEntries(Number(e.target.value))} min={1} className="w-full px-2 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800" />
              </div>
              <label className="flex items-center gap-2">
                <input type="checkbox" checked={ltmScopeEnabled} onChange={(e) => setLtmScopeEnabled(e.target.checked)} className="rounded text-indigo-600" />
                <span className="text-sm text-gray-700 dark:text-gray-300">{t('agentSettings.scopeEnabled')}</span>
              </label>
              <p className="text-[10px] text-gray-400 -mt-2">{t('agentSettings.scopeEnabledHint')}</p>
              <LtmCategoryCheckboxGroup
                label={t('agentSettings.globalCategories')}
                hint={t('agentSettings.globalCategoriesHint')}
                value={ltmGlobalCategories}
                onChange={setLtmGlobalCategories}
                labelFor={(cat) => t(`agentSettings.ltmCategory.${cat}`)}
              />
              <LtmCategoryCheckboxGroup
                label={t('agentSettings.pinnedCategories')}
                hint={t('agentSettings.pinnedCategoriesHint')}
                value={ltmPinnedCategories}
                onChange={setLtmPinnedCategories}
                labelFor={(cat) => t(`agentSettings.ltmCategory.${cat}`)}
              />
              <LtmCategoryCheckboxGroup
                label={t('agentSettings.recallCategories')}
                hint={t('agentSettings.recallCategoriesHint')}
                value={ltmRecallCategories}
                onChange={setLtmRecallCategories}
                labelFor={(cat) => t(`agentSettings.ltmCategory.${cat}`)}
              />
            </>
          )}
        </div>
      </section>

      {ltmEnabled && (
        <section>
          <h2 className="text-lg font-semibold text-gray-900 dark:text-gray-100 mb-4">{t('agentSettings.memoryEntries')}</h2>
          <AgentMemoryPage agentId={agent.id} />
        </section>
      )}

      <section>
        <h2 className="text-lg font-semibold text-gray-900 dark:text-gray-100 mb-4">{t('agentSettings.channels')}</h2>
        <div className="space-y-3">
          {channelBindings.map((cb, idx) => {
            const schema = channelTypes.find((ct) => ct.type === cb.type);
            return (
              <div key={idx} className="p-3 rounded-lg border border-gray-200 dark:border-gray-700 space-y-2">
                <div className="flex items-center justify-between">
                  <span className="text-sm font-medium text-gray-800 dark:text-gray-200">{schema?.label ?? cb.type}</span>
                  <button onClick={() => setChannelBindings(channelBindings.filter((_, i) => i !== idx))} className="text-red-500 hover:text-red-700"><Trash2 size={14} /></button>
                </div>
                {schema && Object.entries(schema.config_schema).map(([key, field]) => (
                  <div key={key}>
                    <label className="block text-xs text-gray-500 mb-0.5">{key}{field.required && ' *'}</label>
                    <input
                      type={field.secret ? 'password' : 'text'}
                      value={cb.config[key] ?? ''}
                      onChange={(e) => {
                        const updated = [...channelBindings];
                        updated[idx] = { ...cb, config: { ...cb.config, [key]: e.target.value } };
                        setChannelBindings(updated);
                      }}
                      className="w-full px-2 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
                    />
                  </div>
                ))}
                <div className="flex items-center gap-2 text-xs text-gray-400">
                  <span>Webhook URL: /api/webhook/{cb.type}</span>
                  <button onClick={() => { navigator.clipboard.writeText(`${window.location.origin}/api/webhook/${cb.type}`); addToast('success', 'Copied'); }} className="hover:text-gray-600"><Copy size={12} /></button>
                </div>
              </div>
            );
          })}
          {channelTypes.length > 0 && (
            <div className="flex items-end gap-2">
              <div className="flex-1">
                <label className="block text-xs text-gray-500 mb-1">{t('agentSettings.addChannel')}</label>
                <select
                  value={addChannelType}
                  onChange={(e) => { setAddChannelType(e.target.value); setAddChannelConfig({}); }}
                  className="w-full px-2 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
                >
                  <option value="">{t('agentSettings.selectChannelType')}</option>
                  {channelTypes
                    .filter((ct) => !channelBindings.some((cb) => cb.type === ct.type))
                    .map((ct) => <option key={ct.type} value={ct.type}>{ct.label}</option>)}
                </select>
              </div>
              <button
                disabled={!addChannelType}
                onClick={() => {
                  if (!addChannelType) return;
                  setChannelBindings([...channelBindings, { type: addChannelType, config: addChannelConfig }]);
                  setAddChannelType('');
                  setAddChannelConfig({});
                }}
                className="px-3 py-1.5 text-sm bg-indigo-600 text-white rounded hover:bg-indigo-700 disabled:opacity-40 flex items-center gap-1"
              >
                <Plus size={14} /> {t('agentSettings.add')}
              </button>
            </div>
          )}
        </div>
      </section>

      <section>
        <h2 className="text-lg font-semibold text-gray-900 dark:text-gray-100 mb-4">{t('skillWhitelist.title')}</h2>
        <SkillWhitelistEditor whitelist={skillWhitelist} onChange={setSkillWhitelist} />
      </section>

      <button onClick={handleSave} className="flex items-center gap-2 px-6 py-2.5 bg-indigo-600 text-white rounded-lg hover:bg-indigo-700 text-sm font-medium">
        <Save size={16} /> {t('agentSettings.saveSettings')}
      </button>
    </div>
  );
}
