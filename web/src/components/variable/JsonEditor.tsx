import { useState } from 'react';

interface Props {
  value: unknown;
  onChange: (value: unknown) => void;
  rows?: number;
}

export default function JsonEditor({ value, onChange, rows = 4 }: Props) {
  const [raw, setRaw] = useState(() => typeof value === 'string' ? value : JSON.stringify(value, null, 2));
  const [error, setError] = useState<string | null>(null);

  const handleChange = (text: string) => {
    setRaw(text);
    try {
      onChange(JSON.parse(text));
      setError(null);
    } catch {
      setError('Invalid JSON');
      onChange(text);
    }
  };

  return (
    <div>
      <textarea
        value={raw}
        onChange={(e) => handleChange(e.target.value)}
        rows={rows}
        className="w-full px-3 py-2 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 font-mono"
      />
      {error && <p className="text-[10px] text-amber-500 mt-0.5">{error}</p>}
    </div>
  );
}
