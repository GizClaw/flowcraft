/** Built-in long-term memory category ids (aligned with sdk/memory). */
export const LTM_CATEGORY_IDS = [
  'profile',
  'preferences',
  'entities',
  'events',
  'cases',
  'patterns',
] as const;

type Props = {
  label: string;
  hint?: string;
  value: string[];
  onChange: (next: string[]) => void;
  labelFor: (cat: string) => string;
};

export default function LtmCategoryCheckboxGroup({ label, hint, value, onChange, labelFor }: Props) {
  const toggle = (cat: string, checked: boolean) => {
    onChange(checked ? [...value, cat] : value.filter((c) => c !== cat));
  };

  return (
    <div>
      <label className="block text-xs text-gray-500 mb-2">{label}</label>
      <div className="flex flex-wrap gap-2">
        {LTM_CATEGORY_IDS.map((cat) => (
          <label key={cat} className="flex items-center gap-1.5 text-sm text-gray-700 dark:text-gray-300">
            <input
              type="checkbox"
              checked={value.includes(cat)}
              onChange={(e) => toggle(cat, e.target.checked)}
              className="rounded text-indigo-600"
            />
            <span>{labelFor(cat)}</span>
          </label>
        ))}
      </div>
      {hint && value.length === 0 ? <p className="text-[10px] text-gray-400 mt-1">{hint}</p> : null}
    </div>
  );
}
