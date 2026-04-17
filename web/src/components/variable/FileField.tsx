import type { Variable, ValidationError } from '../../types/variable';
import FieldWrapper from './FieldWrapper';

interface Props {
  variable: Variable;
  value: unknown;
  onChange: (value: unknown) => void;
  error?: ValidationError;
}

export default function FileField({ variable, value, onChange, error }: Props) {
  const handleFile = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    const reader = new FileReader();
    reader.onload = () => onChange(reader.result);
    reader.readAsDataURL(file);
  };

  const preview = typeof value === 'string' ? value.slice(0, 60) : null;

  return (
    <FieldWrapper variable={variable} error={error}>
      <input
        type="file"
        onChange={handleFile}
        className="w-full text-sm text-gray-500 file:mr-4 file:py-2 file:px-4 file:rounded-lg file:border-0 file:text-sm file:font-medium file:bg-indigo-50 file:text-indigo-700 dark:file:bg-indigo-950 dark:file:text-indigo-300 hover:file:bg-indigo-100"
      />
      {preview && <p className="text-[10px] text-gray-400 mt-1 truncate">{preview}…</p>}
    </FieldWrapper>
  );
}
