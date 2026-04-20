import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { skillApi, type SkillItem } from '../../utils/api';
import { useToastStore } from '../../store/toastStore';

interface Props {
  whitelist: string[];
  onChange: (whitelist: string[]) => void;
}

export default function SkillWhitelistEditor({ whitelist, onChange }: Props) {
  const { t } = useTranslation();
  const [skills, setSkills] = useState<SkillItem[]>([]);
  const addToast = useToastStore((s) => s.addToast);

  useEffect(() => {
    let alive = true;
    skillApi.list()
      .then((list) => { if (alive) setSkills(list); })
      .catch((err) => {
        if (alive) addToast('error', err instanceof Error ? err.message : t('skillWhitelist.loadFailed'));
      });
    return () => { alive = false; };
  }, [addToast, t]);

  // Skills present in the whitelist that are no longer installed; we surface
  // them so users can prune stale entries instead of being stuck with hidden
  // ghost selections.
  const installedNames = useMemo(() => new Set(skills.map((s) => s.name).filter(Boolean) as string[]), [skills]);
  const ghosts = useMemo(() => whitelist.filter((n) => !installedNames.has(n)), [whitelist, installedNames]);

  const toggleSkill = (name: string) => {
    if (whitelist.includes(name)) {
      onChange(whitelist.filter((s) => s !== name));
    } else {
      onChange([...new Set([...whitelist, name])]);
    }
  };

  const removeGhost = (name: string) => {
    onChange(whitelist.filter((s) => s !== name));
  };

  return (
    <div>
      <p className="text-xs text-gray-500 mb-2">
        {whitelist.length === 0
          ? t('skillWhitelist.allAllowed')
          : t('skillWhitelist.selected', { count: whitelist.length })}
      </p>
      <div className="space-y-1 max-h-48 overflow-y-auto">
        {skills.map((skill) => {
          const name = skill.name ?? '';
          if (!name) return null;
          const isChecked = whitelist.includes(name);
          return (
            <label
              key={name}
              className="flex items-center gap-2 px-2 py-1.5 rounded hover:bg-gray-50 dark:hover:bg-gray-800 cursor-pointer"
            >
              <input
                type="checkbox"
                checked={isChecked}
                onChange={() => toggleSkill(name)}
                className="rounded border-gray-300 text-indigo-600"
              />
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-1.5">
                  <span className="text-sm text-gray-700 dark:text-gray-300 truncate">{name}</span>
                  {skill.builtin && (
                    <span className="text-[10px] px-1 py-0.5 rounded bg-indigo-100 dark:bg-indigo-900/30 text-indigo-600 dark:text-indigo-400 shrink-0">
                      {t('skills.builtin')}
                    </span>
                  )}
                  {skill.enabled === false && (
                    <span className="text-[10px] px-1 py-0.5 rounded bg-gray-200 dark:bg-gray-700 text-gray-500 shrink-0">
                      {t('skills.disabled')}
                    </span>
                  )}
                </div>
                {skill.description && (
                  <p className="text-[10px] text-gray-400 truncate">{skill.description}</p>
                )}
              </div>
            </label>
          );
        })}
        {ghosts.map((name) => (
          <label
            key={`ghost-${name}`}
            className="flex items-center gap-2 px-2 py-1.5 rounded bg-amber-50 dark:bg-amber-900/10"
          >
            <input
              type="checkbox"
              checked
              onChange={() => removeGhost(name)}
              className="rounded border-gray-300 text-indigo-600"
            />
            <div className="min-w-0 flex-1">
              <span className="text-sm text-amber-700 dark:text-amber-400 truncate">{name}</span>
              <span className="text-[10px] text-amber-600 dark:text-amber-500 ml-1">
                {t('skillWhitelist.missing')}
              </span>
            </div>
          </label>
        ))}
        {skills.length === 0 && ghosts.length === 0 && (
          <p className="text-xs text-gray-400 py-2">{t('skillWhitelist.empty')}</p>
        )}
      </div>
    </div>
  );
}
