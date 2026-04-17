import { useEffect, useState } from 'react';
import { skillApi, type SkillItem } from '../../utils/api';

interface Props {
  whitelist: string[];
  onChange: (whitelist: string[]) => void;
}

export default function SkillWhitelistEditor({ whitelist, onChange }: Props) {
  const [skills, setSkills] = useState<SkillItem[]>([]);

  useEffect(() => {
    skillApi.list().then(setSkills).catch(() => {});
  }, []);

  const toggleSkill = (name: string) => {
    if (whitelist.includes(name)) {
      onChange(whitelist.filter((s) => s !== name));
    } else {
      onChange([...whitelist, name]);
    }
  };

  return (
    <div>
      <p className="text-xs text-gray-500 mb-2">
        {whitelist.length === 0 ? 'All installed Skills are allowed' : `${whitelist.length} Skill(s) selected`}
      </p>
      <div className="space-y-1 max-h-48 overflow-y-auto">
        {skills.map((skill) => (
          <label key={skill.name} className="flex items-center gap-2 px-2 py-1.5 rounded hover:bg-gray-50 dark:hover:bg-gray-800 cursor-pointer">
            <input
              type="checkbox"
              checked={whitelist.includes(skill.name)}
              onChange={() => toggleSkill(skill.name)}
              className="rounded border-gray-300 text-indigo-600"
            />
            <div>
              <span className="text-sm text-gray-700 dark:text-gray-300">{skill.name}</span>
              {skill.description && <p className="text-[10px] text-gray-400">{skill.description}</p>}
            </div>
          </label>
        ))}
        {skills.length === 0 && <p className="text-xs text-gray-400 py-2">No Skills installed</p>}
      </div>
    </div>
  );
}
